//go:build !fuse3

package main

import (
	"flag"
	"fmt"
	"os"

	"fvs2d/internal/fusefs"
	"fvs2d/internal/runtime"
)

func main() {
	var mountPoint string
	var blocksDir string
	var debug bool
	var blockSize int
	var probe bool

	flag.StringVar(&mountPoint, "mount", "", "mountpoint")
	flag.StringVar(&blocksDir, "blocks", "", "blocks dir (e.g. /path/to/.fvs2/blocks)")
	flag.BoolVar(&debug, "debug", false, "enable debug")
	flag.IntVar(&blockSize, "block-size", 4096, "CoW block size")
	flag.BoolVar(&probe, "probe", false, "print capability report and exit")
	flag.Parse()

	flatpak := runtime.DetectFlatpak()
	devFuse := runtime.DevFuseAccessible()
	fmount := runtime.FusermountAvailable()

	if probe {
		fmt.Printf("flatpak=%v\n", flatpak)
		fmt.Printf("dev_fuse_accessible=%v\n", devFuse)
		fmt.Printf("fusermount_available=%v\n", fmount)
		return
	}

	if mountPoint == "" {
		fmt.Fprintln(os.Stderr, "ERR: --mount is required")
		os.Exit(2)
	}

	if !devFuse || !fmount {
		fmt.Fprintf(os.Stderr, "ERR: cannot mount (dev_fuse_accessible=%v fusermount_available=%v)\n", devFuse, fmount)
		os.Exit(1)
	}

	s, err := fusefs.New(fusefs.Config{MountPoint: mountPoint, Debug: debug, BlocksDir: blocksDir, BlockSize: blockSize})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	if err := s.MountAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(1)
	}
}
