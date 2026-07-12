package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	core "fvs-v2-core"
	fvsrepo "fvs2/repo"
	"fvs2d/fvs2dpb"
	"fvs2d/internal/runtime"
)

const (
	daemonVersion = "1"
	apiVersion    = "v1"
)

// supportedOperations lists the Fvs2d RPC methods this build implements, for
// simple client-side capability checks via Probe.
var supportedOperations = []string{
	"Probe", "InitRepository",
	"Commit", "CommitStream", "ListCommits", "GetCommit",
	"Restore", "RestoreStream",
	"ListFiles", "GetFile", "Diff",
	"CreateMount", "GetMount", "ListMounts", "Unmount", "Shutdown",
}

// mount is a live FUSE mount tracked by the manager.
type mount struct {
	id       string
	spec     *fvs2dpb.MountSpec
	server   *fuse.Server
	point    string
	resolved []*fvs2dpb.ResolvedLayer
	nodes    uint64
	at       time.Time
}

func (m *mount) proto() *fvs2dpb.Mount {
	return &fvs2dpb.Mount{
		Id:             m.id,
		Spec:           m.spec,
		ResolvedLayers: m.resolved,
		NodeCount:      m.nodes,
		MountedAt:      timestamppb.New(m.at),
	}
}

// mountManager owns the registry of live mounts. All methods are safe for
// concurrent use.
type mountManager struct {
	mu      sync.Mutex
	mounts  map[string]*mount
	started time.Time
	guard   *pathGuard
	logf    func(format string, args ...any)
}

func newMountManager(guard *pathGuard, logf func(format string, args ...any)) *mountManager {
	if guard == nil {
		guard = &pathGuard{}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &mountManager{mounts: map[string]*mount{}, started: time.Now(), guard: guard, logf: logf}
}

func newMountID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func selFromLayer(l *fvs2dpb.Layer) layerSel {
	sel := layerSel{repo: l.GetRepositoryPath()}
	switch rev := l.GetRevision().GetSelector().(type) {
	case *fvs2dpb.CommitSelector_StateIdOrPrefix:
		sel.state = rev.StateIdOrPrefix
	case *fvs2dpb.CommitSelector_Branch:
		sel.branch = rev.Branch
	}
	return sel
}

func selFromSelector(repoPath string, sel *fvs2dpb.CommitSelector) layerSel {
	out := layerSel{repo: repoPath}
	switch rev := sel.GetSelector().(type) {
	case *fvs2dpb.CommitSelector_StateIdOrPrefix:
		out.state = rev.StateIdOrPrefix
	case *fvs2dpb.CommitSelector_Branch:
		out.branch = rev.Branch
	}
	return out
}

func (mgr *mountManager) create(spec *fvs2dpb.MountSpec) (*fvs2dpb.Mount, error) {
	if spec.GetMountPoint() == "" {
		return nil, status.Error(codes.InvalidArgument, "mount_point is required")
	}
	if len(spec.GetLayers()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one layer is required")
	}

	sels := make([]layerSel, 0, len(spec.GetLayers()))
	for _, l := range spec.GetLayers() {
		if l.GetRepositoryPath() == "" {
			return nil, status.Error(codes.InvalidArgument, "layer repository_path is required")
		}
		canon, err := mgr.guard.check(l.GetRepositoryPath())
		if err != nil {
			return nil, mapError(err)
		}
		sel := selFromLayer(l)
		sel.repo = canon
		sels = append(sels, sel)
	}

	// Resolve and load each layer's commit exactly once here, then hand the
	// resolved data to buildMergedTreeFromRepos instead of letting it
	// re-resolve/re-load every layer a second time.
	resolvedCommits := make([]resolvedCommit, 0, len(sels))
	for _, sel := range sels {
		rc, err := resolveLayer(sel)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "resolve layer %s: %v", sel.repo, err)
		}
		resolvedCommits = append(resolvedCommits, rc)
	}

	resolved := make([]*fvs2dpb.ResolvedLayer, 0, len(resolvedCommits))
	for _, rc := range resolvedCommits {
		resolved = append(resolved, &fvs2dpb.ResolvedLayer{
			RepositoryPath: rc.repo,
			StateId:        rc.stateID,
			BlocksPath:     filepath.Join(rc.repo, ".fvs2", "blocks"),
			BlockSize:      uint32(rc.blockSize),
		})
	}

	tree, err := buildMergedTreeFromRepos(resolvedCommits, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build tree: %v", err)
	}

	upper := spec.GetUpperPath()
	if upper != "" {
		canon, err := mgr.guard.check(upper)
		if err != nil {
			return nil, mapError(err)
		}
		upper = canon
		if err := os.MkdirAll(upper, 0o755); err != nil {
			return nil, status.Errorf(codes.Internal, "upper dir: %v", err)
		}
	}

	root := newFuseRoot(tree, upper)
	server, err := fs.Mount(spec.GetMountPoint(), root, &fs.Options{
		MountOptions:   fuse.MountOptions{Debug: spec.GetDebug(), FsName: "fvs2d", Name: "fvs2d"},
		RootStableAttr: &fs.StableAttr{Ino: 1, Gen: 1},
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}

	id, err := newMountID()
	if err != nil {
		_ = server.Unmount()
		return nil, status.Errorf(codes.Internal, "mount id: %v", err)
	}

	m := &mount{
		id:       id,
		spec:     spec,
		server:   server,
		point:    spec.GetMountPoint(),
		resolved: resolved,
		nodes:    uint64(len(tree.nodes)),
		at:       time.Now(),
	}

	mgr.mu.Lock()
	mgr.mounts[id] = m
	mgr.mu.Unlock()
	return m.proto(), nil
}

func (mgr *mountManager) get(id string) (*fvs2dpb.Mount, error) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	m, ok := mgr.mounts[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no mount with id %q", id)
	}
	return m.proto(), nil
}

func (mgr *mountManager) list() []*fvs2dpb.Mount {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	out := make([]*fvs2dpb.Mount, 0, len(mgr.mounts))
	for _, m := range mgr.mounts {
		out = append(out, m.proto())
	}
	return out
}

// detach unmounts a single mount. It must be called without the lock held for
// the underlying FUSE server, so we remove it from the registry first.
func (mgr *mountManager) detach(m *mount, lazy bool) error {
	if lazy {
		if err := lazyUnmount(m.point); err == nil {
			return nil
		}
	}
	return m.server.Unmount()
}

func (mgr *mountManager) unmount(id string, lazy bool) error {
	mgr.mu.Lock()
	m, ok := mgr.mounts[id]
	if ok {
		delete(mgr.mounts, id)
	}
	mgr.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "no mount with id %q", id)
	}
	if err := mgr.detach(m, lazy); err != nil {
		return status.Errorf(codes.Internal, "unmount %s: %v", m.point, err)
	}
	return nil
}

// unmountAll detaches every mount; used on shutdown. Failures are logged
// (not silently discarded) but do not stop the sweep: a stuck mount must not
// prevent the daemon from unwinding the rest.
func (mgr *mountManager) unmountAll(lazy bool) {
	mgr.mu.Lock()
	all := make([]*mount, 0, len(mgr.mounts))
	for id, m := range mgr.mounts {
		all = append(all, m)
		delete(mgr.mounts, id)
	}
	mgr.mu.Unlock()
	for _, m := range all {
		if err := mgr.detach(m, lazy); err != nil {
			mgr.logf("manager: unmount %s (%s) failed: %v\n", m.id, m.point, err)
		}
	}
}

// fvs2dService adapts the mountManager to the gRPC Fvs2d service. shutdown is
// invoked to stop the daemon after the in-flight Shutdown reply is sent.
type fvs2dService struct {
	fvs2dpb.UnimplementedFvs2DServer
	mgr      *mountManager
	shutdown func(lazy bool)
	guard    *pathGuard
}

// checkPath canonicalizes a client-supplied path and rejects it with
// codes.PermissionDenied if the daemon has an allowed root configured and
// the path resolves outside it.
func (s *fvs2dService) checkPath(p string) (string, error) {
	return s.guard.check(p)
}

func (s *fvs2dService) Probe(context.Context, *emptypb.Empty) (*fvs2dpb.ProbeResponse, error) {
	resp := &fvs2dpb.ProbeResponse{
		DaemonVersion:        daemonVersion,
		Pid:                  uint32(os.Getpid()),
		StartedAt:            timestamppb.New(s.mgr.started),
		FuseBackendAvailable: true,
		DevFuseAccessible:    runtime.DevFuseAccessible(),
		FusermountAvailable:  runtime.FusermountAvailable(),
		RunningInFlatpak:     runtime.DetectFlatpak(),
		ApiVersion:           apiVersion,
		MinRepoFormat:        uint32(fvsrepo.MinFormat),
		MaxRepoFormat:        uint32(fvsrepo.CurrentFormat),
		SupportedOperations:  supportedOperations,
	}
	if s.guard.root != "" {
		root := s.guard.root
		resp.WorkDir = &root
	}
	return resp, nil
}

func (s *fvs2dService) InitRepository(_ context.Context, req *fvs2dpb.InitRepositoryRequest) (*fvs2dpb.Repository, error) {
	if req.GetRepositoryPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	repository, err := fvsrepo.Init(root, int(req.GetBlockSize()))
	if err != nil {
		return nil, mapError(err)
	}
	return &fvs2dpb.Repository{RepositoryPath: repository.Path, BlockSize: uint32(repository.BlockSize)}, nil
}

// commitResponse builds the Commit reply, reporting the state's real message
// and whether the commit was actually new. A no-op commit (Created==false)
// keeps the previous state's own message, which req's requested message may
// not match, so that case alone is worth the extra DescribeState lookup.
func commitResponse(repoPath string, res fvsrepo.CommitResult, requestedMessage string) *fvs2dpb.Commit {
	msg := requestedMessage
	if !res.Created {
		if detail, err := fvsrepo.DescribeState(repoPath, res.StateID); err == nil {
			msg = detail.Message
		}
	}
	return &fvs2dpb.Commit{
		RepositoryPath: repoPath,
		StateId:        res.StateID,
		CreatedAt:      timestamppb.New(res.CreatedAt),
		FileCount:      uint64(res.FileCount),
		Message:        msg,
		Created:        res.Created,
	}
}

func (s *fvs2dService) Commit(ctx context.Context, req *fvs2dpb.CommitRequest) (*fvs2dpb.Commit, error) {
	if req.GetRepositoryPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	res, err := fvsrepo.CommitContext(ctx, root, req.GetMessage(), req.GetAllowEmpty(), nil)
	if err != nil {
		return nil, mapError(err)
	}
	return commitResponse(root, res, req.GetMessage()), nil
}

// CommitStream is Commit with per-file progress. fvs2's CommitContext writes
// one "hashing: <path>" line per file to its verbose io.Writer; lineWriter
// turns each complete line into a Progress message as it arrives, so a
// client sees real incremental progress rather than just a start/end pair.
func (s *fvs2dService) CommitStream(req *fvs2dpb.CommitRequest, stream fvs2dpb.Fvs2D_CommitStreamServer) error {
	if req.GetRepositoryPath() == "" {
		return status.Error(codes.InvalidArgument, "repository_path is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return mapError(err)
	}
	if err := stream.Send(&fvs2dpb.Progress{Phase: "hashing", Message: "starting commit"}); err != nil {
		return err
	}
	var n int64
	lw := &lineWriter{onLine: func(line string) {
		n++
		// Best-effort: a Send failure here just means the client stopped
		// listening. The commit itself, driven by ctx, is what matters.
		_ = stream.Send(&fvs2dpb.Progress{Phase: "hashing", Current: n, Message: strings.TrimPrefix(line, "hashing: ")})
	}}
	res, err := fvsrepo.CommitContext(stream.Context(), root, req.GetMessage(), req.GetAllowEmpty(), lw)
	if err != nil {
		return mapError(err)
	}
	return stream.Send(&fvs2dpb.Progress{Phase: "done", Done: true, ResultCommit: commitResponse(root, res, req.GetMessage())})
}

// commitSummary builds a CommitSummary, populating file_count via
// DescribeState (repo.State does not carry it).
func commitSummary(repoPath string, st fvsrepo.State) *fvs2dpb.CommitSummary {
	fileCount := uint64(0)
	if detail, err := fvsrepo.DescribeState(repoPath, st.ID); err == nil {
		fileCount = uint64(detail.FileCount)
	}
	return &fvs2dpb.CommitSummary{
		RepositoryPath: repoPath,
		StateId:        st.ID,
		CreatedAt:      timestamppb.New(st.CreatedAt),
		Message:        st.Message,
		FileCount:      fileCount,
	}
}

func (s *fvs2dService) ListCommits(_ context.Context, req *fvs2dpb.ListCommitsRequest) (*fvs2dpb.ListCommitsResponse, error) {
	if req.GetRepositoryPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	states, err := fvsrepo.States(root)
	if err != nil {
		return nil, mapError(err)
	}

	start := 0
	if tok := req.GetPageToken(); tok != "" {
		n, convErr := strconv.Atoi(tok)
		if convErr != nil || n < 0 || n > len(states) {
			return nil, status.Error(codes.InvalidArgument, "invalid page_token")
		}
		start = n
	}
	end := len(states)
	nextToken := ""
	if size := int(req.GetPageSize()); size > 0 && start+size < len(states) {
		end = start + size
		nextToken = strconv.Itoa(end)
	}

	out := make([]*fvs2dpb.CommitSummary, 0, end-start)
	for _, st := range states[start:end] {
		out = append(out, commitSummary(root, st))
	}
	return &fvs2dpb.ListCommitsResponse{Commits: out, NextPageToken: nextToken}, nil
}

func (s *fvs2dService) GetCommit(_ context.Context, req *fvs2dpb.GetCommitRequest) (*fvs2dpb.Commit, error) {
	if req.GetRepositoryPath() == "" || req.GetStateIdOrPrefix() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path and state_id_or_prefix are required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	detail, err := fvsrepo.DescribeState(root, req.GetStateIdOrPrefix())
	if err != nil {
		return nil, mapError(err)
	}
	return &fvs2dpb.Commit{
		RepositoryPath: root,
		StateId:        detail.ID,
		CreatedAt:      timestamppb.New(detail.CreatedAt),
		FileCount:      uint64(detail.FileCount),
		Message:        detail.Message,
		Created:        true,
	}, nil
}

func (s *fvs2dService) Restore(ctx context.Context, req *fvs2dpb.RestoreRequest) (*fvs2dpb.RestoreResponse, error) {
	if req.GetRepositoryPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path is required")
	}
	if req.GetStateIdOrPrefix() == "" {
		return nil, status.Error(codes.InvalidArgument, "state_id_or_prefix is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	dest := req.GetDestinationPath()
	if dest != "" {
		dest, err = s.checkPath(dest)
		if err != nil {
			return nil, mapError(err)
		}
	}
	// RestoreContext locks the repo and writes atomically (temp file + fsync
	// + rename) internally; nothing extra to do here.
	res, err := fvsrepo.RestoreContext(ctx, root, req.GetStateIdOrPrefix(), fvsrepo.RestoreOptions{
		To:    dest,
		Clean: req.GetClean(),
		Reset: req.GetReset_(),
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &fvs2dpb.RestoreResponse{StateId: res.StateID, DestinationPath: res.Dest}, nil
}

// RestoreStream is Restore with per-file progress, parsed the same way as
// CommitStream (see its comment).
func (s *fvs2dService) RestoreStream(req *fvs2dpb.RestoreRequest, stream fvs2dpb.Fvs2D_RestoreStreamServer) error {
	if req.GetRepositoryPath() == "" {
		return status.Error(codes.InvalidArgument, "repository_path is required")
	}
	if req.GetStateIdOrPrefix() == "" {
		return status.Error(codes.InvalidArgument, "state_id_or_prefix is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return mapError(err)
	}
	dest := req.GetDestinationPath()
	if dest != "" {
		dest, err = s.checkPath(dest)
		if err != nil {
			return mapError(err)
		}
	}
	if err := stream.Send(&fvs2dpb.Progress{Phase: "restoring", Message: "starting restore"}); err != nil {
		return err
	}
	var n int64
	lw := &lineWriter{onLine: func(line string) {
		n++
		_ = stream.Send(&fvs2dpb.Progress{Phase: "restoring", Current: n, Message: strings.TrimPrefix(line, "restoring: ")})
	}}
	res, err := fvsrepo.RestoreContext(stream.Context(), root, req.GetStateIdOrPrefix(), fvsrepo.RestoreOptions{
		To:      dest,
		Clean:   req.GetClean(),
		Reset:   req.GetReset_(),
		Verbose: lw,
	})
	if err != nil {
		return mapError(err)
	}
	return stream.Send(&fvs2dpb.Progress{
		Phase: "done", Done: true,
		ResultRestore: &fvs2dpb.RestoreResponse{StateId: res.StateID, DestinationPath: res.Dest},
	})
}

func (s *fvs2dService) ListFiles(_ context.Context, req *fvs2dpb.ListFilesRequest) (*fvs2dpb.ListFilesResponse, error) {
	if req.GetRepositoryPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path is required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	sel := selFromSelector(root, req.GetRevision())
	id, err := fvsrepo.ResolveCommit(root, sel.state, sel.branch)
	if err != nil {
		return nil, mapError(err)
	}
	if id == "" {
		return nil, status.Error(codes.NotFound, "no commit to list (empty branch or no states)")
	}
	files, err := fvsrepo.StateFiles(root, id)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*fvs2dpb.FileInfo, 0, len(files))
	for _, f := range files {
		out = append(out, &fvs2dpb.FileInfo{Path: f.Path, Mode: f.Mode, Size: f.Size, Link: f.Link})
	}
	return &fvs2dpb.ListFilesResponse{Files: out}, nil
}

const defaultGetFileChunkSize = 256 * 1024

func lookupTreePath(t *fsTree, clean string) *fileNode {
	if clean == "" {
		return t.get(1)
	}
	ino := uint64(1)
	for _, part := range strings.Split(clean, "/") {
		ino = t.lookup(ino, part)
		if ino == 0 {
			return nil
		}
	}
	return t.get(ino)
}

func (s *fvs2dService) GetFile(req *fvs2dpb.GetFileRequest, stream fvs2dpb.Fvs2D_GetFileServer) error {
	if req.GetRepositoryPath() == "" || req.GetPath() == "" {
		return status.Error(codes.InvalidArgument, "repository_path and path are required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return mapError(err)
	}
	rc, err := resolveLayer(selFromSelector(root, req.GetRevision()))
	if err != nil {
		return mapError(err)
	}
	blocks := filepath.Join(root, ".fvs2", "blocks")
	store, err := core.NewDiskBlockStore(blocks)
	if err != nil {
		return mapError(err)
	}
	tree := buildTree(store, rc.blockSize, rc.files)
	clean := strings.Trim(path.Clean("/"+req.GetPath()), "/")
	node := lookupTreePath(tree, clean)
	if node == nil || node.isDir {
		return status.Errorf(codes.NotFound, "file not found: %s", req.GetPath())
	}
	chunkSize := int(req.GetChunkSize())
	if chunkSize <= 0 {
		chunkSize = defaultGetFileChunkSize
	}
	ctx := stream.Context()
	var off int64
	for off < node.size {
		if err := ctx.Err(); err != nil {
			return mapError(err)
		}
		n := chunkSize
		if remaining := node.size - off; int64(n) > remaining {
			n = int(remaining)
		}
		data, err := tree.readAt(node, off, n)
		if err != nil {
			return mapError(err)
		}
		if len(data) == 0 {
			break
		}
		if err := stream.Send(&fvs2dpb.GetFileChunk{Data: data, Offset: off}); err != nil {
			return err
		}
		off += int64(len(data))
	}
	return nil
}

func sameFileEntry(a, b fvsrepo.FileEntry) bool {
	if a.Size != b.Size || a.Mode != b.Mode || a.Link != b.Link || len(a.Blocks) != len(b.Blocks) {
		return false
	}
	for i := range a.Blocks {
		if a.Blocks[i] != b.Blocks[i] {
			return false
		}
	}
	return true
}

func (s *fvs2dService) Diff(_ context.Context, req *fvs2dpb.DiffRequest) (*fvs2dpb.DiffResponse, error) {
	if req.GetRepositoryPath() == "" || req.GetFromState() == "" || req.GetToState() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository_path, from_state and to_state are required")
	}
	root, err := s.checkPath(req.GetRepositoryPath())
	if err != nil {
		return nil, mapError(err)
	}
	fromID, err := fvsrepo.ResolveCommit(root, req.GetFromState(), "")
	if err != nil {
		return nil, mapError(err)
	}
	toID, err := fvsrepo.ResolveCommit(root, req.GetToState(), "")
	if err != nil {
		return nil, mapError(err)
	}
	fromFiles, err := fvsrepo.StateFiles(root, fromID)
	if err != nil {
		return nil, mapError(err)
	}
	toFiles, err := fvsrepo.StateFiles(root, toID)
	if err != nil {
		return nil, mapError(err)
	}

	fm := make(map[string]fvsrepo.FileEntry, len(fromFiles))
	for _, f := range fromFiles {
		fm[f.Path] = f
	}
	tm := make(map[string]fvsrepo.FileEntry, len(toFiles))
	for _, f := range toFiles {
		tm[f.Path] = f
	}

	var changes []*fvs2dpb.FileChange
	for p, tf := range tm {
		if ff, ok := fm[p]; ok {
			if !sameFileEntry(ff, tf) {
				changes = append(changes, &fvs2dpb.FileChange{Path: p, Kind: fvs2dpb.ChangeKind_CHANGE_KIND_MODIFIED, SizeDelta: tf.Size - ff.Size})
			}
		} else {
			changes = append(changes, &fvs2dpb.FileChange{Path: p, Kind: fvs2dpb.ChangeKind_CHANGE_KIND_ADDED, SizeDelta: tf.Size})
		}
	}
	for p, ff := range fm {
		if _, ok := tm[p]; !ok {
			changes = append(changes, &fvs2dpb.FileChange{Path: p, Kind: fvs2dpb.ChangeKind_CHANGE_KIND_REMOVED, SizeDelta: -ff.Size})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return &fvs2dpb.DiffResponse{Changes: changes}, nil
}

func (s *fvs2dService) CreateMount(_ context.Context, req *fvs2dpb.CreateMountRequest) (*fvs2dpb.Mount, error) {
	if req.GetSpec() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	return s.mgr.create(req.GetSpec())
}

func (s *fvs2dService) GetMount(_ context.Context, req *fvs2dpb.GetMountRequest) (*fvs2dpb.Mount, error) {
	return s.mgr.get(req.GetMountId())
}

func (s *fvs2dService) ListMounts(context.Context, *emptypb.Empty) (*fvs2dpb.ListMountsResponse, error) {
	return &fvs2dpb.ListMountsResponse{Mounts: s.mgr.list()}, nil
}

func (s *fvs2dService) Unmount(_ context.Context, req *fvs2dpb.UnmountRequest) (*emptypb.Empty, error) {
	if err := s.mgr.unmount(req.GetMountId(), req.GetMode() == fvs2dpb.UnmountMode_UNMOUNT_MODE_LAZY); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *fvs2dService) Shutdown(_ context.Context, req *fvs2dpb.ShutdownRequest) (*emptypb.Empty, error) {
	s.shutdown(req.GetMode() == fvs2dpb.UnmountMode_UNMOUNT_MODE_LAZY)
	return &emptypb.Empty{}, nil
}

func parseControlAddr(addr string) (network, address string, err error) {
	switch {
	case addr == "":
		return "", "", fmt.Errorf("empty control address")
	case strings.HasPrefix(addr, "unix:"):
		return "unix", strings.TrimPrefix(addr, "unix:"), nil
	case strings.HasPrefix(addr, "tcp:"):
		return "tcp", strings.TrimPrefix(addr, "tcp:"), nil
	case strings.HasPrefix(addr, "/"):
		return "unix", addr, nil
	default:
		return "tcp", addr, nil
	}
}

// isLoopbackAddr reports whether a "host:port" TCP address resolves to a
// loopback interface. Unparseable hosts are treated as non-loopback (fail
// closed): startManagerServer then requires --insecure-tcp and --token.
func isLoopbackAddr(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serverConfig holds the control server's transport-security settings,
// plumbed from main.go's --insecure-tcp/--token flags.
type serverConfig struct {
	insecureTCP bool
	token       string
}

// startManagerServer serves the Fvs2d mount-manager API (plus standard health)
// on addr and returns the running server.
//
// Unix sockets are the trusted default: filesystem permissions gate access,
// so no token is required. TCP is different: a loopback bind (127.0.0.1/::1)
// without a token is allowed for local dev but logs a prominent warning,
// since any local process or user can then reach the control API. A
// non-loopback bind is refused outright unless the operator opts in with
// both --insecure-tcp and a --token, and every RPC is then gated by a
// constant-time token-comparison interceptor.
func startManagerServer(addr string, svc *fvs2dService, cfg serverConfig, logf func(format string, args ...any)) (*grpc.Server, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	network, address, err := parseControlAddr(addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("control: remove stale socket: %w", err)
		}
	}

	var opts []grpc.ServerOption
	if network == "tcp" {
		loopback := isLoopbackAddr(address)
		if !loopback {
			if !cfg.insecureTCP {
				return nil, fmt.Errorf("control: refusing non-loopback TCP bind %s without --insecure-tcp", address)
			}
			if cfg.token == "" {
				return nil, fmt.Errorf("control: refusing non-loopback TCP bind %s without --token", address)
			}
		} else if cfg.token == "" {
			logf("WARNING: control server listening on loopback TCP %s without a token; any local process can connect\n", address)
		}
		if cfg.token != "" {
			opts = append(opts,
				grpc.ChainUnaryInterceptor(tokenUnaryInterceptor(cfg.token)),
				grpc.ChainStreamInterceptor(tokenStreamInterceptor(cfg.token)),
			)
		}
	}

	lis, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("control: listen %s %s: %w", network, address, err)
	}
	if network == "unix" {
		if err := os.Chmod(address, 0o600); err != nil {
			_ = lis.Close()
			return nil, fmt.Errorf("control: chmod socket: %w", err)
		}
	}
	gs := grpc.NewServer(opts...)
	fvs2dpb.RegisterFvs2DServer(gs, svc)
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("fvs2d.v1.Fvs2d", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	go func() {
		if err := gs.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			logf("manager: control server stopped: %v\n", err)
		}
	}()
	return gs, nil
}
