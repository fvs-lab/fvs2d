package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fvs-lab/fvs2d/fvs2dpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type testMountServer struct {
	done chan struct{}
	once sync.Once
	fail error
}

func (s *testMountServer) Wait() { <-s.done }
func (s *testMountServer) Unmount() error {
	if s.fail != nil {
		return s.fail
	}
	s.once.Do(func() { close(s.done) })
	return nil
}
func TestFailedUnmountKeepsRegistryAndCaches(t *testing.T) {
	mgr := newMountManager(nil, nil)
	server := &testMountServer{done: make(chan struct{}), fail: errors.New("busy")}
	m := &mount{id: "test", server: server, spec: &fvs2dpb.MountSpec{}, treeKey: "tree"}
	mgr.mounts[m.id] = m
	mgr.trees["tree"] = &cachedTree{refs: 1}
	if err := mgr.unmount(m.id, false); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	if _, err := mgr.get(m.id); err != nil || mgr.trees["tree"].refs != 1 {
		t.Fatal("failed unmount lost state")
	}
	server.fail = nil
	if err := mgr.unmount(m.id, false); err != nil {
		t.Fatal(err)
	}
	mgr.forgetMount(m)
	if len(mgr.mounts) != 0 || len(mgr.trees) != 0 {
		t.Fatal("mount cache leak")
	}
}
func TestShutdownDoesNotSucceedWithBusyMount(t *testing.T) {
	mgr := newMountManager(nil, nil)
	server := &testMountServer{done: make(chan struct{}), fail: errors.New("busy")}
	mgr.mounts["test"] = &mount{id: "test", server: server}
	stopped := false
	service := &fvs2dService{mgr: mgr, shutdown: func(bool) { stopped = true }}
	if _, err := service.Shutdown(context.Background(), &fvs2dpb.ShutdownRequest{}); err == nil || stopped {
		t.Fatal("shutdown hid an unmount error")
	}
}
func TestProbeDeclaresPlatformMountBackend(t *testing.T) {
	mgr := newMountManager(nil, nil)
	service := &fvs2dService{mgr: mgr, guard: &pathGuard{}}
	response, err := service.Probe(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetMountBackend() == "" {
		t.Fatal("missing backend capability")
	}
	if response.GetMountBackend() == "fskit" && (response.GetMinimumMacosVersion() != "26.0" || response.GetFuseBackendAvailable() || response.GetSupportsLazyUnmount()) {
		t.Fatalf("inconsistent FSKit capabilities: %v", response)
	}
}

func TestMountPathsProtectRepositoryStorage(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	blocks := filepath.Join(repository, ".fvs2/blocks")
	point := filepath.Join(root, "mount")
	upper := filepath.Join(root, "upper")
	for _, p := range []string{blocks, point, upper} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	options := mountOptions{Point: point, Upper: upper, Layers: []Layer{{Repository: repository, Blocks: blocks}}}
	if err := options.validatePaths(); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{repository, blocks, point, root} {
		options.Upper = unsafe
		if err := options.validatePaths(); err == nil {
			t.Fatalf("accepted upper %s", unsafe)
		}
	}
	options.Upper = upper
	options.Point = repository
	if err := options.validatePaths(); err == nil {
		t.Fatal("mount hides its own storage")
	}
}

func TestManagerProbeDiagnosesUnavailableDaemon(t *testing.T) {
	if _, err := probeManager("unix:"+filepath.Join(t.TempDir(), "absent.sock"), ""); err == nil || !strings.Contains(err.Error(), "mount manager unavailable") {
		t.Fatalf("unavailable daemon: %v", err)
	}
}
func TestManagerProbeUsesRealControlServer(t *testing.T) {
	t.Chdir(t.TempDir())
	service := &fvs2dService{mgr: newMountManager(nil, nil), guard: &pathGuard{}}
	address := "unix:control.sock"
	server, err := startManagerServer(address, service, serverConfig{}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	response, err := probeManager(address, "")
	if err != nil || response.GetMountBackend() == "" {
		t.Fatalf("probe %+v %v", response, err)
	}
}
