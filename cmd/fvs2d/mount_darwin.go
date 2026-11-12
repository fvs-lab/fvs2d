//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fvs-lab/fvs2d/internal/engine"
	"github.com/fvs-lab/fvs2d/internal/runtime"
	"golang.org/x/sys/unix"
)

type darwinMount struct {
	point string
	fsid  unix.Fsid
	done  chan struct{}
	once  sync.Once
}

func (m *darwinMount) Wait() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			var st unix.Statfs_t
			if unix.Statfs(m.point, &st) != nil || st.Fsid != m.fsid {
				m.once.Do(func() { close(m.done) })
				return
			}
		}
	}
}
func (m *darwinMount) Unmount() error {
	if err := platformUnmount(m.point, false); err != nil {
		return err
	}
	m.once.Do(func() { close(m.done) })
	return nil
}
func platformUnmount(point string, lazy bool) error {
	if lazy {
		return fmt.Errorf("FSKit does not provide Linux lazy unmount: %w", syscall.ENOTSUP)
	}
	point, err := filepath.Abs(point)
	if err != nil {
		return err
	}
	output, err := exec.Command("/sbin/umount", point).CombinedOutput()
	if err != nil {
		return fmt.Errorf("FSKit unmount: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
func startMount(o mountOptions) (mountServer, error) {
	if err := o.validatePaths(); err != nil {
		return nil, err
	}
	caps := runtime.Probe()
	if !caps.Available {
		return nil, fmt.Errorf("%s", caps.Diagnostic)
	}
	source := o.Source
	var err error
	if source == "" {
		source, err = engine.CommonSource(o.Layers, o.Upper)
		if err != nil {
			return nil, err
		}
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	point, err := filepath.Abs(o.Point)
	if err != nil {
		return nil, err
	}
	point, err = filepath.EvalSymlinks(point)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(source, point); err == nil && filepath.IsLocal(rel) {
		return nil, fmt.Errorf("mount point must be outside the FSKit source directory")
	}
	entries, err := os.ReadDir(point)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("mount point must be an empty directory: %w", syscall.ENOTEMPTY)
	}
	lock, err := os.OpenFile(filepath.Join(source, ".fvs-mount.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("another mount is configuring this FSKit resource: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := engine.WriteResourceConfig(source, o.Layers, o.Upper, o.ClearPrivilegedBits); err != nil {
		return nil, err
	}
	// Preflight uses the exact configuration and engine loaded by the extension.
	e, err := engine.LoadResource(source)
	if err != nil {
		return nil, err
	}
	e.Close()
	var before unix.Statfs_t
	if err := unix.Statfs(point, &before); err != nil {
		return nil, err
	}
	output, err := exec.Command("/sbin/mount", "-t", "fvs", source, point).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("FSKit mount failed: %s; check extension enablement, folder access and 'fvs2d -probe': %w", strings.TrimSpace(string(output)), err)
	}
	var after unix.Statfs_t
	if err := unix.Statfs(point, &after); err != nil {
		return nil, err
	}
	if after.Fsid == before.Fsid {
		return nil, fmt.Errorf("mount returned success without attaching a filesystem: %w", syscall.EIO)
	}
	return &darwinMount{point: point, fsid: after.Fsid, done: make(chan struct{})}, nil
}
