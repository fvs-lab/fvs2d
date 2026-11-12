//go:build linux

package main

import (
	"fmt"
	"os/exec"
	"sync"

	"github.com/fvs-lab/fvs2d/internal/engine"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type linuxMount struct {
	*fuse.Server
	close func() error
	once  sync.Once
}

func (m *linuxMount) Wait() { m.Server.Wait(); m.once.Do(func() { m.close() }) }
func (m *linuxMount) Unmount() error {
	if err := m.Server.Unmount(); err != nil {
		return err
	}
	m.Wait()
	return nil
}
func startMount(o mountOptions) (mountServer, error) {
	if err := o.validatePaths(); err != nil {
		return nil, err
	}
	root := engine.NewFuseRootWithOptions(o.Tree, o.Upper, o.ClearPrivilegedBits)
	if err := root.Err(); err != nil {
		root.Close()
		return nil, err
	}
	server, err := fs.Mount(o.Point, root, &fs.Options{MountOptions: fuse.MountOptions{Debug: o.Debug, FsName: "fvs2d", Name: "fvs2d"}, NullPermissions: true, RootStableAttr: &fs.StableAttr{Ino: 1, Gen: 1}})
	if err != nil {
		root.Close()
		return nil, err
	}
	return &linuxMount{Server: server, close: root.Close}, nil
}
func platformUnmount(point string, lazy bool) error {
	flag := "-u"
	if lazy {
		flag = "-uz"
	}
	for _, command := range []string{"fusermount3", "fusermount"} {
		if path, err := exec.LookPath(command); err == nil {
			return exec.Command(path, flag, "--", point).Run()
		}
	}
	return fmt.Errorf("fusermount not found")
}
