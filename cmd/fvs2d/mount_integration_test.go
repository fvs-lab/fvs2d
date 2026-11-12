package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	fvsrepo "github.com/fvs-lab/fvs2/repo"
	"github.com/fvs-lab/fvs2d/fvs2dpb"
	"github.com/fvs-lab/fvs2d/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestManagerRealMountLifecycle(t *testing.T) {
	if os.Getenv("FVS2D_TEST_REAL_MOUNT") != "1" {
		t.Skip("set FVS2D_TEST_REAL_MOUNT=1 to require real mounts")
	}
	if caps := runtime.Probe(); !caps.Available {
		t.Fatalf("real mount backend unavailable: %s", caps.Diagnostic)
	}
	root := t.TempDir()
	t.Chdir(root)
	source := filepath.Join(root, "source")
	repository := filepath.Join(source, "repo")
	upper := filepath.Join(source, "upper")
	point := filepath.Join(root, "mount")
	for _, dir := range []string{upper, point} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fvsrepo.Init(repository, 4096); err != nil {
		t.Fatal(err)
	}
	writer, err := fvsrepo.BeginSnapshot(repository, fvsrepo.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(fvsrepo.Entry{Path: "file", Kind: fvsrepo.EntryFile, Mode: 0o644, Size: 4}, bytes.NewReader([]byte("base"))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(fvsrepo.Entry{Path: "no-permissions", Kind: fvsrepo.EntryFile}, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	guard, err := newPathGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := newMountManager(guard, nil)
	service := &fvs2dService{mgr: manager, guard: guard, shutdown: func(bool) {}}
	server, err := startManagerServer("unix:control.sock", service, serverConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	defer manager.unmountAll(false)
	connection, err := grpc.NewClient("passthrough:///manager-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "control.sock")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := fvs2dpb.NewFvs2DClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := &fvs2dpb.MountSpec{MountPoint: point, Layers: []*fvs2dpb.Layer{{RepositoryPath: repository}}, UpperPath: &upper}
	if runtime.Probe().Backend == "fskit" {
		spec.SourcePath = &source
	}
	mounted, err := client.CreateMount(ctx, &fvs2dpb.CreateMountRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(point, "file")); err != nil || string(data) != "base" {
		t.Fatalf("mounted content %q %v", data, err)
	}
	if info, err := os.Lstat(filepath.Join(point, "no-permissions")); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("zero permission bits changed: %v %v", info, err)
	}
	created, err := os.OpenFile(filepath.Join(point, "created-without-permissions"), os.O_CREATE|os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := created.Write([]byte("created")); err != nil {
		created.Close()
		t.Fatal(err)
	}
	if err := created.Truncate(3); err != nil {
		created.Close()
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(point, "file"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	listing, err := client.ListMounts(ctx, &emptypb.Empty{})
	if err != nil || len(listing.GetMounts()) != 1 {
		t.Fatalf("registry: %v %v", listing, err)
	}
	if _, err := client.GetMount(ctx, &fvs2dpb.GetMountRequest{MountId: mounted.Id}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Unmount(ctx, &fvs2dpb.UnmountRequest{MountId: mounted.Id}); err != nil {
		t.Fatal(err)
	}
	if len(manager.list()) != 0 {
		t.Fatal("unmounted volume remained registered")
	}
	mounted, err = client.CreateMount(ctx, &fvs2dpb.CreateMountRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(point, "file")); err != nil || string(data) != "upper" {
		t.Fatalf("remounted upper %q %v", data, err)
	}
	if err := platformUnmount(point, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(manager.list()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(manager.list()) != 0 {
		t.Fatal("external unmount remained registered")
	}
}
