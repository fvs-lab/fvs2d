package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/fvs-lab/fvs2d/internal/runtime"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func run() error {
	var mountPoint, blocksDir, repoDir, stateSel, branchSel, upperDir, controlAddr string
	var rootDir, token, sourcePath, unmountPoint string
	var lowers stringList
	var debug, probe, insecureTCP bool
	var blockSize int

	flag.StringVar(&sourcePath, "source", "", "FSKit resource directory containing all layers, blocks and upper (macOS)")
	flag.StringVar(&unmountPoint, "unmount", "", "unmount a mount point and exit")
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
	flag.StringVar(&rootDir, "root", "", "allowed-root sandbox: reject any client-supplied path outside this directory (empty = arbitrary paths, current behavior)")
	flag.BoolVar(&insecureTCP, "insecure-tcp", false, "allow the control server to bind a non-loopback TCP address (requires --token)")
	flag.StringVar(&token, "token", os.Getenv("FVS2D_TOKEN"), "shared control-API token for TCP (default: $FVS2D_TOKEN); unix sockets never require it")
	flag.Parse()
	_ = blockSize

	if unmountPoint != "" {
		return platformUnmount(unmountPoint, false)
	}
	capabilities := runtime.Probe()
	flatpak := runtime.DetectFlatpak()
	devFuse := runtime.DevFuseAccessible()
	fmount := runtime.FusermountAvailable()
	if probe {
		fmt.Printf("mount_backend=%s\nmount_backend_available=%v\nminimum_macos_version=%s\nsupports_lazy_unmount=%v\ndiagnostic=%s\n", capabilities.Backend, capabilities.Available, capabilities.MinimumMacOS, capabilities.LazyUnmount, capabilities.Diagnostic)
		fmt.Printf("flatpak=%v\n", flatpak)
		fmt.Printf("dev_fuse_accessible=%v\n", devFuse)
		fmt.Printf("fusermount_available=%v\n", fmount)
		if controlAddr != "" {
			remote, err := probeManager(controlAddr, token)
			if err != nil {
				fmt.Println("manager_available=false")
				return err
			}
			fmt.Printf("manager_available=true\nmanager_mount_backend=%s\nmanager_mount_backend_available=%v\nmanager_diagnostic=%s\n", remote.GetMountBackend(), remote.GetMountBackendAvailable(), remote.GetMountDiagnostic())
		}

		return nil
	}
	// Manager mode: with no initial mount, serve the Fvs2d mount-manager over
	// gRPC so clients can create and unmount mounts at runtime.
	if mountPoint == "" {
		if controlAddr == "" {
			return fmt.Errorf("--mount is required (or --control to run as a mount manager)")
		}
		return runManager(controlAddr, rootDir, serverConfig{insecureTCP: insecureTCP, token: token})
	}
	if controlAddr != "" {
		return fmt.Errorf("--control cannot be combined with --mount; run the manager without --mount")
	}
	if !capabilities.Available {
		return fmt.Errorf("cannot mount: %s", capabilities.Diagnostic)
	}

	var tree *Tree
	var resolved []Layer
	var err error
	if len(lowers) > 0 {
		sels := make([]LayerSelector, 0, len(lowers))
		for _, layer := range lowers {
			sels = append(sels, ParseLayerSelector(layer))
		}
		resolved = make([]Layer, 0, len(sels))
		for _, sel := range sels {
			rc, rerr := ResolveLayer(sel)
			if rerr != nil {
				return fmt.Errorf("layer %s: %w", sel.Repository, rerr)
			}
			resolved = append(resolved, rc)
		}
		tree, err = BuildMergedTreeFromRepos(resolved, blocksDir)
	} else {
		var layer Layer
		layer, err = ResolveLayer(LayerSelector{Repository: repoDir, State: stateSel, Branch: branchSel})
		if err == nil {
			resolved = []Layer{layer}
			tree, err = BuildMergedTreeFromRepos(resolved, blocksDir)
		}
	}
	if err != nil {
		return err
	}
	if blocksDir != "" {
		for i := range resolved {
			resolved[i].Blocks = blocksDir
		}
	}
	if upperDir != "" {
		if err := os.MkdirAll(upperDir, 0o755); err != nil {
			return fmt.Errorf("upper dir: %w", err)
		}
	}

	server, err := startMount(mountOptions{Point: mountPoint, Source: sourcePath, Tree: tree, Layers: resolved, Upper: upperDir, Debug: debug})
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
func runManager(addr, rootDir string, cfg serverConfig) error {
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) }

	guard, err := newPathGuard(rootDir)
	if err != nil {
		return fmt.Errorf("root: %w", err)
	}
	if guard.root == "" {
		fmt.Fprintln(os.Stderr, "WARNING: no --root configured; the daemon will operate on arbitrary filesystem paths supplied by any client")
	}

	mgr := newMountManager(guard, logf)
	shutdownReq := make(chan bool, 1)
	svc := &fvs2dService{
		mgr:   mgr,
		guard: guard,
		shutdown: func(lazy bool) {
			select {
			case shutdownReq <- lazy:
			default:
			}
		},
	}

	server, err := startManagerServer(addr, svc, cfg, logf)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "manager: serving on %s\n", addr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		err := mgr.unmountAll(false)
		server.Stop()
		if err != nil {
			return err
		}
	case lazy := <-shutdownReq:
		err := mgr.unmountAll(lazy)
		server.GracefulStop()
		if err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(1)
	}
}
