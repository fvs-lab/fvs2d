package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

	"fvs2d/internal/fvs2dpb"
	"fvs2d/internal/runtime"
)

const daemonVersion = "1"

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
}

func newMountManager() *mountManager {
	return &mountManager{mounts: map[string]*mount{}, started: time.Now()}
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
	case *fvs2dpb.RevisionSelector_StateIdOrPrefix:
		sel.state = rev.StateIdOrPrefix
	case *fvs2dpb.RevisionSelector_Branch:
		sel.branch = rev.Branch
	}
	return sel
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
		sels = append(sels, selFromLayer(l))
	}

	resolved := make([]*fvs2dpb.ResolvedLayer, 0, len(sels))
	for _, sel := range sels {
		id, err := resolveCommit(sel.repo, sel.state, sel.branch)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "resolve layer %s: %v", sel.repo, err)
		}
		doc, err := loadCommitDoc(sel.repo, id)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load layer %s: %v", sel.repo, err)
		}
		resolved = append(resolved, &fvs2dpb.ResolvedLayer{
			RepositoryPath: sel.repo,
			StateId:        id,
			BlocksPath:     filepath.Join(sel.repo, ".fvs2", "blocks"),
			BlockSize:      uint32(doc.BlockSize),
		})
	}

	tree, err := buildMergedTreeFromRepos(sels, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build tree: %v", err)
	}

	upper := spec.GetUpperPath()
	if upper != "" {
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

// unmountAll detaches every mount; used on shutdown.
func (mgr *mountManager) unmountAll(lazy bool) {
	mgr.mu.Lock()
	all := make([]*mount, 0, len(mgr.mounts))
	for id, m := range mgr.mounts {
		all = append(all, m)
		delete(mgr.mounts, id)
	}
	mgr.mu.Unlock()
	for _, m := range all {
		_ = mgr.detach(m, lazy)
	}
}

// fvs2dService adapts the mountManager to the gRPC Fvs2d service. shutdown is
// invoked to stop the daemon after the in-flight Shutdown reply is sent.
type fvs2dService struct {
	fvs2dpb.UnimplementedFvs2DServer
	mgr      *mountManager
	shutdown func(lazy bool)
}

func (s *fvs2dService) Probe(context.Context, *emptypb.Empty) (*fvs2dpb.ProbeResponse, error) {
	return &fvs2dpb.ProbeResponse{
		DaemonVersion:        daemonVersion,
		Pid:                  uint32(os.Getpid()),
		StartedAt:            timestamppb.New(s.mgr.started),
		FuseBackendAvailable: true,
		DevFuseAccessible:    runtime.DevFuseAccessible(),
		FusermountAvailable:  runtime.FusermountAvailable(),
		RunningInFlatpak:     runtime.DetectFlatpak(),
	}, nil
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

// startManagerServer serves the Fvs2d mount-manager API (plus standard health)
// on addr and returns the running server.
func startManagerServer(addr string, svc *fvs2dService) (*grpc.Server, error) {
	network, address, err := parseControlAddr(addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("control: remove stale socket: %w", err)
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
	gs := grpc.NewServer()
	fvs2dpb.RegisterFvs2DServer(gs, svc)
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	go func() { _ = gs.Serve(lis) }()
	return gs, nil
}
