package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"fvs2d/fvs2dpb"
)

// TestPathGuardRejectsEscape verifies the allowed-root sandbox (item 2):
// paths outside the configured root are rejected with PermissionDenied
// instead of being served.
func TestPathGuardRejectsEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()

	guard, err := newPathGuard(allowed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.check(outside); err == nil {
		t.Fatal("expected escape from allowed root to be rejected")
	} else if !errors.Is(err, errPathEscape) {
		t.Fatalf("err = %v, want errPathEscape", err)
	}

	inside := filepath.Join(allowed, "repo")
	canon, err := guard.check(inside)
	if err != nil {
		t.Fatalf("path inside root rejected: %v", err)
	}
	if canon == "" {
		t.Fatal("expected a canonicalized path")
	}
}

func TestPathGuardUnrestricted(t *testing.T) {
	guard, err := newPathGuard("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.check(t.TempDir()); err != nil {
		t.Fatalf("unrestricted guard should allow any path: %v", err)
	}
}

// TestServiceRejectsPathOutsideRoot exercises the guard end-to-end through a
// gRPC handler (item 2/9).
func TestServiceRejectsPathOutsideRoot(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	guard, err := newPathGuard(allowed)
	if err != nil {
		t.Fatal(err)
	}
	service := &fvs2dService{mgr: newMountManager(guard, nil), guard: guard, shutdown: func(bool) {}}

	_, err = service.InitRepository(context.Background(), &fvs2dpb.InitRepositoryRequest{RepositoryPath: outside})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", status.Code(err), err)
	}
}

func TestMapErrorCodes(t *testing.T) {
	tests := []struct {
		err  error
		want codes.Code
	}{
		{errPathEscape, codes.PermissionDenied},
		{context.Canceled, codes.Canceled},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{io.EOF, codes.FailedPrecondition},
	}
	for _, tt := range tests {
		got := status.Code(mapError(tt.err))
		if got != tt.want {
			t.Errorf("mapError(%v) code = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestTokenInterceptorRejectsMissingOrWrongToken(t *testing.T) {
	interceptor := tokenUnaryInterceptor("secret")
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return nil, nil
	}

	// No metadata at all.
	if _, err := interceptor(context.Background(), nil, nil, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no metadata: code = %v, want Unauthenticated", status.Code(err))
	}

	// Wrong token.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(tokenMetadataKey, "wrong"))
	if _, err := interceptor(ctx, nil, nil, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token: code = %v, want Unauthenticated", status.Code(err))
	}
	if handlerCalled {
		t.Fatal("handler must not run when the token is wrong")
	}

	// Correct token.
	ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(tokenMetadataKey, "secret"))
	if _, err := interceptor(ctx, nil, nil, handler); err != nil {
		t.Fatalf("correct token: unexpected error %v", err)
	}
	if !handlerCalled {
		t.Fatal("handler should run once the token matches")
	}
}

func TestStartManagerServerRefusesNonLoopbackTCPWithoutFlags(t *testing.T) {
	// A non-loopback address without --insecure-tcp/--token must fail at
	// startup, not silently serve unauthenticated.
	if _, err := startManagerServer("tcp:203.0.113.1:0", &fvs2dService{}, serverConfig{}, nil); err == nil {
		t.Fatal("expected non-loopback TCP bind to be refused")
	}
	if _, err := startManagerServer("tcp:203.0.113.1:0", &fvs2dService{}, serverConfig{insecureTCP: true}, nil); err == nil {
		t.Fatal("expected non-loopback TCP bind without a token to be refused even with --insecure-tcp")
	}
}

func TestNoOpCommitKeepsPreviousMessage(t *testing.T) {
	root := t.TempDir()
	service := &fvs2dService{mgr: newMountManager(nil, nil), guard: &pathGuard{}, shutdown: func(bool) {}}
	ctx := context.Background()

	if _, err := service.InitRepository(ctx, &fvs2dpb.InitRepositoryRequest{RepositoryPath: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "first message"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetCreated() {
		t.Fatal("first commit should be Created")
	}

	// No changes, no allow_empty: this should be a no-op, and its reply
	// should report the *previous* commit's own message/id, not the
	// (irrelevant) message passed on this call.
	second, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "ignored message"})
	if err != nil {
		t.Fatal(err)
	}
	if second.GetCreated() {
		t.Fatal("second commit should be a no-op (Created=false)")
	}
	if second.GetStateId() != first.GetStateId() {
		t.Fatalf("no-op state id = %s, want %s", second.GetStateId(), first.GetStateId())
	}
	if second.GetMessage() != "first message" {
		t.Fatalf("no-op message = %q, want %q", second.GetMessage(), "first message")
	}
}

func TestListFilesGetFileDiff(t *testing.T) {
	root := t.TempDir()
	service := &fvs2dService{mgr: newMountManager(nil, nil), guard: &pathGuard{}, shutdown: func(bool) {}}
	ctx := context.Background()

	if _, err := service.InitRepository(ctx, &fvs2dpb.InitRepositoryRequest{RepositoryPath: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "first"})
	if err != nil {
		t.Fatal(err)
	}

	files, err := service.ListFiles(ctx, &fvs2dpb.ListFilesRequest{RepositoryPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(files.GetFiles()) != 1 || files.GetFiles()[0].GetPath() != "file" || files.GetFiles()[0].GetSize() != int64(len("hello world")) {
		t.Fatalf("ListFiles = %+v", files.GetFiles())
	}

	stream := &fakeGetFileStream{ctx: ctx}
	if err := service.GetFile(&fvs2dpb.GetFileRequest{RepositoryPath: root, Path: "file"}, stream); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, c := range stream.sent {
		got = append(got, c.GetData()...)
	}
	if string(got) != "hello world" {
		t.Fatalf("GetFile content = %q, want %q", got, "hello world")
	}

	if err := os.WriteFile(filepath.Join(root, "file"), []byte("hello world, changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new"), []byte("new file"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "second"})
	if err != nil {
		t.Fatal(err)
	}

	diff, err := service.Diff(ctx, &fvs2dpb.DiffRequest{RepositoryPath: root, FromState: first.GetStateId(), ToState: second.GetStateId()})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]fvs2dpb.ChangeKind{}
	for _, c := range diff.GetChanges() {
		kinds[c.GetPath()] = c.GetKind()
	}
	if kinds["file"] != fvs2dpb.ChangeKind_CHANGE_KIND_MODIFIED {
		t.Errorf("file kind = %v, want MODIFIED", kinds["file"])
	}
	if kinds["new"] != fvs2dpb.ChangeKind_CHANGE_KIND_ADDED {
		t.Errorf("new kind = %v, want ADDED", kinds["new"])
	}
}

func TestListCommitsPagination(t *testing.T) {
	root := t.TempDir()
	service := &fvs2dService{mgr: newMountManager(nil, nil), guard: &pathGuard{}, shutdown: func(bool) {}}
	ctx := context.Background()

	if _, err := service.InitRepository(ctx, &fvs2dpb.InitRepositoryRequest{RepositoryPath: root}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(root, "file"), []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "c", AllowEmpty: true}); err != nil {
			t.Fatal(err)
		}
	}

	page1, err := service.ListCommits(ctx, &fvs2dpb.ListCommitsRequest{RepositoryPath: root, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.GetCommits()) != 2 || page1.GetNextPageToken() == "" {
		t.Fatalf("page1 = %d commits, next=%q", len(page1.GetCommits()), page1.GetNextPageToken())
	}
	page2, err := service.ListCommits(ctx, &fvs2dpb.ListCommitsRequest{RepositoryPath: root, PageSize: 2, PageToken: page1.GetNextPageToken()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.GetCommits()) != 1 || page2.GetNextPageToken() != "" {
		t.Fatalf("page2 = %d commits, next=%q", len(page2.GetCommits()), page2.GetNextPageToken())
	}
}

// fakeGetFileStream is a minimal fvs2dpb.Fvs2D_GetFileServer for exercising
// GetFile without a real gRPC connection.
type fakeGetFileStream struct {
	fvs2dpb.Fvs2D_GetFileServer
	ctx  context.Context
	sent []*fvs2dpb.GetFileChunk
}

func (f *fakeGetFileStream) Send(c *fvs2dpb.GetFileChunk) error {
	f.sent = append(f.sent, c)
	return nil
}

func (f *fakeGetFileStream) Context() context.Context { return f.ctx }

// fakeProgressStream is a minimal fvs2dpb.Fvs2D_CommitStreamServer /
// Fvs2D_RestoreStreamServer for exercising CommitStream/RestoreStream
// without a real gRPC connection.
type fakeProgressStream struct {
	fvs2dpb.Fvs2D_CommitStreamServer
	ctx  context.Context
	sent []*fvs2dpb.Progress
}

func (f *fakeProgressStream) Send(p *fvs2dpb.Progress) error {
	f.sent = append(f.sent, p)
	return nil
}

func (f *fakeProgressStream) Context() context.Context { return f.ctx }

func TestCommitStreamAndRestoreStream(t *testing.T) {
	root := t.TempDir()
	service := &fvs2dService{mgr: newMountManager(nil, nil), guard: &pathGuard{}, shutdown: func(bool) {}}
	ctx := context.Background()

	if _, err := service.InitRepository(ctx, &fvs2dpb.InitRepositoryRequest{RepositoryPath: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}

	commitStream := &fakeProgressStream{ctx: ctx}
	if err := service.CommitStream(&fvs2dpb.CommitRequest{RepositoryPath: root, Message: "first"}, commitStream); err != nil {
		t.Fatal(err)
	}
	if len(commitStream.sent) < 2 {
		t.Fatalf("expected at least a start and done message, got %d", len(commitStream.sent))
	}
	last := commitStream.sent[len(commitStream.sent)-1]
	if !last.GetDone() || last.GetResultCommit() == nil || !last.GetResultCommit().GetCreated() {
		t.Fatalf("final Progress = %+v", last)
	}
	stateID := last.GetResultCommit().GetStateId()

	if err := os.WriteFile(filepath.Join(root, "a"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreStream := &fakeProgressStream{ctx: ctx}
	err := service.RestoreStream(&fvs2dpb.RestoreRequest{RepositoryPath: root, StateIdOrPrefix: stateID, Clean: true}, restoreStream)
	if err != nil {
		t.Fatal(err)
	}
	restoreLast := restoreStream.sent[len(restoreStream.sent)-1]
	if !restoreLast.GetDone() || restoreLast.GetResultRestore().GetStateId() != stateID {
		t.Fatalf("final restore Progress = %+v", restoreLast)
	}
	content, err := os.ReadFile(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one" {
		t.Fatalf("restored content = %q, want %q", content, "one")
	}
}

func TestCreateMountRejectsEmptyLayerPath(t *testing.T) {
	guard, err := newPathGuard("")
	if err != nil {
		t.Fatal(err)
	}
	mgr := newMountManager(guard, nil)
	_, err = mgr.create(&fvs2dpb.MountSpec{
		MountPoint: t.TempDir(),
		Layers:     []*fvs2dpb.Layer{{RepositoryPath: ""}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// TestCreateMountRejectsMountPointOutsideRoot verifies that, with a
// sandboxing --root configured, the FUSE mount_point itself (not just the
// layer/upper paths) is checked against the allowed root. Without this, a
// client could ask the daemon to mount an arbitrary merged view of
// attacker-chosen repos at any filesystem location (e.g. over another
// user's directory), completely bypassing the --root sandbox.
func TestCreateMountRejectsMountPointOutsideRoot(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	guard, err := newPathGuard(allowed)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newMountManager(guard, nil)
	_, err = mgr.create(&fvs2dpb.MountSpec{
		MountPoint: filepath.Join(outside, "mnt"),
		Layers:     []*fvs2dpb.Layer{{RepositoryPath: filepath.Join(allowed, "repo")}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", status.Code(err), err)
	}
}

// TestPathEscapeErrorDoesNotLeakPaths verifies that a rejected path escape
// does not echo the resolved allowed root or the client-supplied absolute
// path back in the gRPC error message, which would otherwise leak the
// daemon's filesystem layout to any client probing the sandbox boundary.
func TestPathEscapeErrorDoesNotLeakPaths(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	guard, err := newPathGuard(allowed)
	if err != nil {
		t.Fatal(err)
	}
	_, checkErr := guard.check(outside)
	if checkErr == nil {
		t.Fatal("expected escape to be rejected")
	}
	grpcErr := mapError(checkErr)
	msg := grpcErr.Error()
	if strings.Contains(msg, allowed) {
		t.Fatalf("error message leaks allowed root: %q", msg)
	}
	if strings.Contains(msg, outside) {
		t.Fatalf("error message leaks client-supplied path: %q", msg)
	}
	if status.Code(grpcErr) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(grpcErr))
	}
}
