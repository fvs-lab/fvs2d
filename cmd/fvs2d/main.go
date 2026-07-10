package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"fvs2d/internal/runtime"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func lazyUnmount(mountPoint string) error {
	for _, command := range []string{"fusermount3", "fusermount"} {
		if path, err := exec.LookPath(command); err == nil {
			return exec.Command(path, "-uz", mountPoint).Run()
		}
	}
	return fmt.Errorf("fusermount not found")
}

func run() error {
	var mountPoint, blocksDir, repoDir, stateSel, branchSel, upperDir, controlAddr string
	var lowers stringList
	var debug, probe bool
	var blockSize int

	flag.StringVar(&mountPoint, "mount", "", "mountpoint")
	flag.StringVar(&repoDir, "repo", ".", "repo root containing .fvs2")
	flag.StringVar(&stateSel, "state", "", "state id/prefix to mount (default: HEAD)")
	flag.StringVar(&branchSel, "branch", "", "branch to mount (default: HEAD)")
	flag.Var(&lowers, "lower", "lower layer repo (repeatable, low-to-high): repo | repo@state | repo#branch")
	flag.StringVar(&upperDir, "upper", "", "writable upper layer dir (enables writes; omit for read-only)")
	flag.StringVar(&blocksDir, "blocks", "", "blocks dir override (default: <repo>/.fvs2/blocks)")
	flag.BoolVar(&debug, "debug", false, "enable debug")
	flag.IntVar(&blockSize, "block-size", 4096, "fallback block size (overridden by the commit)")
	flag.BoolVar(&probe, "probe", false, "print capability report and exit")
	flag.StringVar(&controlAddr, "control", "", "enable gRPC control server: unix:/path.sock or tcp:host:port (empty = disabled)")
	flag.Parse()
	_ = blockSize

	flatpak := runtime.DetectFlatpak()
	devFuse := runtime.DevFuseAccessible()
	fmount := runtime.FusermountAvailable()
	if probe {
		fmt.Printf("flatpak=%v\n", flatpak)
		fmt.Printf("dev_fuse_accessible=%v\n", devFuse)
		fmt.Printf("fusermount_available=%v\n", fmount)
		return nil
	}
	// Manager mode: with no initial mount, serve the Fvs2d mount-manager over
	// gRPC so clients can create and unmount mounts at runtime.
	if mountPoint == "" {
		if controlAddr == "" {
			return fmt.Errorf("--mount is required (or --control to run as a mount manager)")
		}
		return runManager(controlAddr)
	}
	if controlAddr != "" {
		return fmt.Errorf("--control cannot be combined with --mount; run the manager without --mount")
	}
	if !devFuse || !fmount {
		return fmt.Errorf("cannot mount (dev_fuse_accessible=%v fusermount_available=%v)", devFuse, fmount)
	}

	var tree *fsTree
	var err error
	if len(lowers) > 0 {
		sels := make([]layerSel, 0, len(lowers))
		for _, layer := range lowers {
			sels = append(sels, parseLayerSel(layer))
		}
		tree, err = buildMergedTreeFromRepos(sels, blocksDir)
	} else {
		tree, err = buildTreeFromRepo(repoDir, stateSel, branchSel, blocksDir)
	}
	if err != nil {
		return err
	}
	if upperDir != "" {
		if err := os.MkdirAll(upperDir, 0o755); err != nil {
			return fmt.Errorf("upper dir: %w", err)
		}
	}

	root := newFuseRoot(tree, upperDir)
	server, err := fs.Mount(mountPoint, root, &fs.Options{
		MountOptions:   fuse.MountOptions{Debug: debug, FsName: "fvs2d", Name: "fvs2d"},
		RootStableAttr: &fs.StableAttr{Ino: 1, Gen: 1},
	})
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	done := make(chan struct{})
	go func() {
		server.Wait()
		close(done)
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		err = server.Unmount()
	case <-done:
	}
	return err
}

// runManager runs fvs2d as a persistent mount manager: it serves the Fvs2d
// gRPC API and holds no mount of its own until a client creates one.
func runManager(addr string) error {
	mgr := newMountManager()
	shutdownReq := make(chan bool, 1)
	svc := &fvs2dService{
		mgr: mgr,
		shutdown: func(lazy bool) {
			select {
			case shutdownReq <- lazy:
			default:
			}
		},
	}

	server, err := startManagerServer(addr, svc)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "manager: serving on %s\n", addr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		mgr.unmountAll(false)
		server.Stop()
	case lazy := <-shutdownReq:
		mgr.unmountAll(lazy)
		server.GracefulStop()
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(1)
	}
}
