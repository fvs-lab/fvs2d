//go:build linux

package engine

import (
	"context"
	"errors"
	"io"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fuseState struct {
	*Engine
	root *fuseNode
}
type fuseNode struct {
	fs.Inode
	State *fuseState
}

func NewFuseRoot(lower *Tree, upper string) *fuseNode {
	return NewFuseRootWithOptions(lower, upper, false)
}
func NewFuseRootWithOptions(lower *Tree, upper string, clearPrivilegedBits bool) *fuseNode {
	e, err := New(lower, upper, clearPrivilegedBits)
	if err != nil {
		e = &Engine{initError: err}
	}
	s := &fuseState{Engine: e}
	s.root = &fuseNode{State: s}
	return s.root
}
func (n *fuseNode) Err() error   { return n.State.initError }
func (n *fuseNode) Close() error { return n.State.Close() }
func (n *fuseNode) path() string { return n.Path(&n.State.root.Inode) }
func (n *fuseNode) child(ctx context.Context, a Attr) *fs.Inode {
	return n.NewInode(ctx, &fuseNode{State: n.State}, fs.StableAttr{Mode: a.Mode & syscall.S_IFMT, Ino: a.ID, Gen: 1})
}
func fillAttr(out *fuse.Attr, a Attr) {
	out.Ino = a.ID
	out.Mode = a.Mode
	out.Size = uint64(a.Size)
	out.Uid = a.UID
	out.Gid = a.GID
	out.Nlink = 1
	if a.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		out.Nlink = 2
	}
	mt := time.Unix(0, a.MtimeNS)
	out.SetTimes(&mt, &mt, &mt)
}
func (n *fuseNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validName(name) {
		return nil, syscall.EINVAL
	}
	a, err := n.State.Lookup(joinPath(n.path(), name))
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return n.child(ctx, a), 0
}
func (n *fuseNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if h, ok := fh.(*fuseFile); ok {
		return h.Getattr(ctx, out)
	}
	a, err := n.State.Lookup(n.path())
	if err != nil {
		return fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return 0
}
func (n *fuseNode) Readdir(context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.State.ReadDir(n.path())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	out := make([]fuse.DirEntry, 0, len(entries))
	for _, a := range entries {
		out = append(out, fuse.DirEntry{Name: a.Name, Mode: a.Mode, Ino: a.ID})
	}
	return fs.NewListDirStream(out), 0
}
func (n *fuseNode) Readlink(context.Context) ([]byte, syscall.Errno) {
	target, err := n.State.Readlink(n.path())
	return []byte(target), fs.ToErrno(err)
}
func (n *fuseNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	var h *Handle
	var err error
	if flags&syscall.O_ACCMODE == syscall.O_WRONLY {
		h, err = n.State.OpenWriteOnly(n.path())
	} else {
		h, err = n.State.Open(n.path(), flags&syscall.O_ACCMODE != syscall.O_RDONLY)
	}
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	if flags&syscall.O_TRUNC != 0 {
		zero := int64(0)
		if _, err := n.State.Setattr(n.path(), Changes{Size: &zero}); err != nil {
			h.Close()
			return nil, 0, fs.ToErrno(err)
		}
	}
	return &fuseFile{h}, 0, 0
}
func (n *fuseNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if h, ok := fh.(*fuseFile); ok {
		return h.Read(ctx, dest, off)
	}
	return nil, syscall.EBADF
}
func (n *fuseNode) Setattr(_ context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	c := Changes{}
	if v, ok := in.GetMode(); ok {
		c.Mode = &v
	}
	if v, ok := in.GetSize(); ok {
		if v > 1<<63-1 {
			return syscall.EFBIG
		}
		v := int64(v)
		c.Size = &v
	}
	if v, ok := in.GetUID(); ok {
		v := int(v)
		c.UID = &v
	}
	if v, ok := in.GetGID(); ok {
		v := int(v)
		c.GID = &v
	}
	if v, ok := in.GetATime(); ok {
		v := v.UnixNano()
		c.AtimeNS = &v
	}
	if v, ok := in.GetMTime(); ok {
		v := v.UnixNano()
		c.MtimeNS = &v
	}
	var a Attr
	var err error
	if file, ok := fh.(*fuseFile); ok {
		a, err = file.Handle.Setattr(c)
	} else {
		a, err = n.State.Setattr(n.path(), c)
	}
	if err != nil {
		return fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return 0
}
func (n *fuseNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if !validName(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	p := joinPath(n.path(), name)
	a, h, err := n.State.CreateOpen(p, mode)
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return n.child(ctx, a), &fuseFile{h}, 0, 0
}
func (n *fuseNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validName(name) {
		return nil, syscall.EINVAL
	}
	a, err := n.State.Create(joinPath(n.path(), name), "dir", "", mode)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return n.child(ctx, a), 0
}
func (n *fuseNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validName(name) {
		return nil, syscall.EINVAL
	}
	a, err := n.State.Create(joinPath(n.path(), name), "symlink", target, 0o777)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return n.child(ctx, a), 0
}
func (n *fuseNode) Unlink(_ context.Context, name string) syscall.Errno {
	if !validName(name) {
		return syscall.EINVAL
	}
	return fs.ToErrno(n.State.Remove(joinPath(n.path(), name), false))
}
func (n *fuseNode) Rmdir(_ context.Context, name string) syscall.Errno {
	if !validName(name) {
		return syscall.EINVAL
	}
	return fs.ToErrno(n.State.Remove(joinPath(n.path(), name), true))
}
func (n *fuseNode) Rename(_ context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if !validName(name) || !validName(newName) {
		return syscall.EINVAL
	}
	to, ok := newParent.(*fuseNode)
	if !ok || to.State != n.State {
		return syscall.EXDEV
	}
	if flags != 0 {
		return syscall.EINVAL
	}
	return fs.ToErrno(n.State.Rename(joinPath(n.path(), name), joinPath(to.path(), newName)))
}

type fuseFile struct{ *Handle }

func (f *fuseFile) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.Handle.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}
func (f *fuseFile) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := f.Handle.WriteAt(data, off)
	return uint32(n), fs.ToErrno(err)
}
func (f *fuseFile) Getattr(_ context.Context, out *fuse.AttrOut) syscall.Errno {
	a, err := f.Handle.Stat()
	if err != nil {
		return fs.ToErrno(err)
	}
	fillAttr(&out.Attr, a)
	return 0
}
func (f *fuseFile) Fsync(context.Context, uint32) syscall.Errno { return fs.ToErrno(f.Handle.Sync()) }
func (f *fuseFile) Flush(context.Context) syscall.Errno         { return 0 }
func (f *fuseFile) Release(context.Context) syscall.Errno       { return fs.ToErrno(f.Handle.Close()) }
