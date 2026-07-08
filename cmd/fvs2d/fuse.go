package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fuseState struct {
	lower *fsTree
	upper string
	root  *fuseNode
	mu    sync.Mutex
	uid   uint32
	gid   uint32
	mtime uint64
}

type fuseNode struct {
	fs.Inode
	state *fuseState
}

type pathInfo struct {
	lower *fileNode
	upper *syscall.Stat_t
}

func newFuseRoot(lower *fsTree, upper string) *fuseNode {
	s := &fuseState{
		lower: lower,
		upper: upper,
		uid:   uint32(os.Getuid()),
		gid:   uint32(os.Getgid()),
		mtime: uint64(time.Now().Unix()),
	}
	s.root = &fuseNode{state: s}
	return s.root
}

func (n *fuseNode) path() string { return n.Path(&n.state.root.Inode) }

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/')
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func splitPath(p string) (parent, base string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func (s *fuseState) writable() bool            { return s.upper != "" }
func (s *fuseState) upperPath(p string) string { return filepath.Join(s.upper, filepath.FromSlash(p)) }

func (s *fuseState) lowerNode(p string) *fileNode {
	if s.lower == nil {
		return nil
	}
	if p == "" {
		return s.lower.get(1)
	}
	cur := uint64(1)
	for _, part := range strings.Split(p, "/") {
		cur = s.lower.lookup(cur, part)
		if cur == 0 {
			return nil
		}
	}
	return s.lower.get(cur)
}

func (s *fuseState) hasWhiteout(parent, base string) bool {
	if !s.writable() {
		return false
	}
	_, err := os.Lstat(s.upperPath(joinPath(parent, whiteoutPrefix+base)))
	return err == nil
}

func (s *fuseState) stat(p string) pathInfo {
	if p != "" {
		parent, base := splitPath(p)
		if s.hasWhiteout(parent, base) {
			return pathInfo{}
		}
	}
	if s.writable() {
		var st syscall.Stat_t
		if err := syscall.Lstat(s.upperPath(p), &st); err == nil {
			return pathInfo{upper: &st, lower: s.lowerNode(p)}
		}
	}
	return pathInfo{lower: s.lowerNode(p)}
}

func (i pathInfo) exists() bool { return i.upper != nil || i.lower != nil }

func (i pathInfo) mode() uint32 {
	if i.upper != nil {
		return i.upper.Mode
	}
	if i.lower == nil {
		return 0
	}
	switch {
	case i.lower.isDir:
		return syscall.S_IFDIR | i.lower.mode&0o7777
	case i.lower.link != "":
		return syscall.S_IFLNK | 0o777
	default:
		return syscall.S_IFREG | i.lower.mode&0o7777
	}
}

func (i pathInfo) isDir() bool  { return i.mode()&syscall.S_IFMT == syscall.S_IFDIR }
func (i pathInfo) isLink() bool { return i.mode()&syscall.S_IFMT == syscall.S_IFLNK }

func (s *fuseState) fillAttr(out *fuse.Attr, i pathInfo) {
	if i.upper != nil {
		out.FromStat(i.upper)
		return
	}
	out.Mode = i.mode()
	out.Uid = s.uid
	out.Gid = s.gid
	out.Atime = s.mtime
	out.Mtime = s.mtime
	out.Ctime = s.mtime
	out.Nlink = 1
	if i.lower != nil {
		out.Ino = i.lower.ino
		out.Size = uint64(i.lower.size)
		if i.lower.isDir {
			out.Nlink = 2
		}
	}
}

func stableAttr(i pathInfo) fs.StableAttr {
	a := fs.StableAttr{Mode: i.mode() & syscall.S_IFMT}
	if i.upper != nil {
		a.Ino, a.Gen = i.upper.Ino, 2
	} else if i.lower != nil {
		a.Ino, a.Gen = i.lower.ino, 1
	}
	return a
}

func (n *fuseNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validName(name) {
		return nil, syscall.EINVAL
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	i := n.state.stat(joinPath(n.path(), name))
	if !i.exists() {
		return nil, syscall.ENOENT
	}
	n.state.fillAttr(&out.Attr, i)
	return n.NewInode(ctx, &fuseNode{state: n.state}, stableAttr(i)), 0
}

func (n *fuseNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if f, ok := fh.(fs.FileGetattrer); ok {
		return f.Getattr(ctx, out)
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	i := n.state.stat(n.path())
	if !i.exists() {
		return syscall.ENOENT
	}
	n.state.fillAttr(&out.Attr, i)
	return 0
}

func (s *fuseState) dirEntries(p string) ([]fuse.DirEntry, syscall.Errno) {
	i := s.stat(p)
	if !i.exists() {
		return nil, syscall.ENOENT
	}
	if !i.isDir() {
		return nil, syscall.ENOTDIR
	}
	names := map[string]struct{}{}
	if i.lower != nil && i.lower.isDir {
		for name := range i.lower.children {
			if !s.hasWhiteout(p, name) {
				names[name] = struct{}{}
			}
		}
	}
	if s.writable() {
		entries, err := os.ReadDir(s.upperPath(p))
		if err != nil && !os.IsNotExist(err) {
			return nil, fs.ToErrno(err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), whiteoutPrefix) {
				names[entry.Name()] = struct{}{}
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	out := make([]fuse.DirEntry, 0, len(sorted))
	for _, name := range sorted {
		child := s.stat(joinPath(p, name))
		if child.exists() {
			out = append(out, fuse.DirEntry{Name: name, Mode: child.mode(), Ino: stableAttr(child).Ino})
		}
	}
	return out, 0
}

func (n *fuseNode) Readdir(context.Context) (fs.DirStream, syscall.Errno) {
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	entries, errno := n.state.dirEntries(n.path())
	if errno != 0 {
		return nil, errno
	}
	return fs.NewListDirStream(entries), 0
}

func (n *fuseNode) Readlink(context.Context) ([]byte, syscall.Errno) {
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	p := n.path()
	i := n.state.stat(p)
	if !i.exists() || !i.isLink() {
		return nil, syscall.EINVAL
	}
	if i.upper != nil {
		target, err := os.Readlink(n.state.upperPath(p))
		return []byte(target), fs.ToErrno(err)
	}
	return []byte(i.lower.link), 0
}

type lowerFile struct {
	state *fuseState
	node  *fileNode
}

func (f *lowerFile) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := f.state.lower.readAt(f.node, off, len(dest))
	if err != nil {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(data), 0
}

func (s *fuseState) copyUp(p string) error {
	if _, err := os.Lstat(s.upperPath(p)); err == nil {
		return nil
	}
	lower := s.lowerNode(p)
	if lower == nil || lower.isDir {
		return nil
	}
	up := s.upperPath(p)
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		return err
	}
	if lower.link != "" {
		return os.Symlink(lower.link, up)
	}
	f, err := os.OpenFile(up, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(lower.mode&0o7777))
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(up)
		}
	}()
	for off := int64(0); off < lower.size; {
		data, err := s.lower.readAt(lower, off, 1<<20)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return io.ErrUnexpectedEOF
		}
		written, err := f.Write(data)
		if err != nil {
			return err
		}
		if written != len(data) {
			return io.ErrShortWrite
		}
		off += int64(written)
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func openLoopback(p string, flags uint32, mode uint32) (fs.FileHandle, syscall.Errno) {
	flags &^= syscall.O_APPEND | fuse.FMODE_EXEC
	fd, err := syscall.Open(p, int(flags), mode)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	return fs.NewLoopbackFile(fd), 0
}

func (n *fuseNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	p := n.path()
	i := n.state.stat(p)
	if !i.exists() {
		return nil, 0, syscall.ENOENT
	}
	write := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	if write && !n.state.writable() {
		return nil, 0, syscall.EROFS
	}
	if write && i.upper == nil {
		if err := n.state.copyUp(p); err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		i = n.state.stat(p)
	}
	if i.upper != nil {
		fh, errno := openLoopback(n.state.upperPath(p), flags, 0)
		return fh, 0, errno
	}
	return &lowerFile{state: n.state, node: i.lower}, 0, 0
}

func (n *fuseNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if f, ok := fh.(fs.FileReader); ok {
		return f.Read(ctx, dest, off)
	}
	return nil, syscall.EBADF
}

func (n *fuseNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if f, ok := fh.(fs.FileSetattrer); ok {
		return f.Setattr(ctx, in, out)
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	if !n.state.writable() {
		return syscall.EROFS
	}
	p := n.path()
	if err := n.state.copyUp(p); err != nil {
		return fs.ToErrno(err)
	}
	fh, errno := openLoopback(n.state.upperPath(p), syscall.O_WRONLY, 0)
	if errno != 0 {
		return errno
	}
	defer fh.(fs.FileReleaser).Release(ctx)
	return fh.(fs.FileSetattrer).Setattr(ctx, in, out)
}

func (s *fuseState) clearWhiteout(parent, base string) error {
	err := os.Remove(s.upperPath(joinPath(parent, whiteoutPrefix+base)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *fuseState) makeWhiteout(parent, base string) error {
	if err := os.MkdirAll(s.upperPath(parent), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.upperPath(joinPath(parent, whiteoutPrefix+base)), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func (n *fuseNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if !validName(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	if !n.state.writable() {
		return nil, nil, 0, syscall.EROFS
	}
	parent := n.path()
	if err := os.MkdirAll(n.state.upperPath(parent), 0o755); err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	hadWhiteout := n.state.hasWhiteout(parent, name)
	if err := n.state.clearWhiteout(parent, name); err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	p := joinPath(parent, name)
	fh, errno := openLoopback(n.state.upperPath(p), flags|syscall.O_CREAT, mode&0o7777)
	if errno != 0 {
		if hadWhiteout {
			_ = n.state.makeWhiteout(parent, name)
		}
		return nil, nil, 0, errno
	}
	i := n.state.stat(p)
	n.state.fillAttr(&out.Attr, i)
	return n.NewInode(ctx, &fuseNode{state: n.state}, stableAttr(i)), fh, 0, 0
}

func (n *fuseNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validName(name) {
		return nil, syscall.EINVAL
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	if !n.state.writable() {
		return nil, syscall.EROFS
	}
	parent := n.path()
	if err := os.MkdirAll(n.state.upperPath(parent), 0o755); err != nil {
		return nil, fs.ToErrno(err)
	}
	hadWhiteout := n.state.hasWhiteout(parent, name)
	if err := n.state.clearWhiteout(parent, name); err != nil {
		return nil, fs.ToErrno(err)
	}
	p := joinPath(parent, name)
	if err := os.Mkdir(n.state.upperPath(p), os.FileMode(mode&0o7777)); err != nil {
		if hadWhiteout {
			_ = n.state.makeWhiteout(parent, name)
		}
		return nil, fs.ToErrno(err)
	}
	i := n.state.stat(p)
	n.state.fillAttr(&out.Attr, i)
	return n.NewInode(ctx, &fuseNode{state: n.state}, stableAttr(i)), 0
}

func (n *fuseNode) remove(name string, dir bool) syscall.Errno {
	if !validName(name) {
		return syscall.EINVAL
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	if !n.state.writable() {
		return syscall.EROFS
	}
	parent := n.path()
	p := joinPath(parent, name)
	i := n.state.stat(p)
	if !i.exists() {
		return syscall.ENOENT
	}
	if dir != i.isDir() {
		if dir {
			return syscall.ENOTDIR
		}
		return syscall.EISDIR
	}
	if dir {
		entries, errno := n.state.dirEntries(p)
		if errno != 0 {
			return errno
		}
		if len(entries) != 0 {
			return syscall.ENOTEMPTY
		}
	}
	whiteout := i.lower != nil
	if whiteout {
		if err := n.state.makeWhiteout(parent, name); err != nil {
			return fs.ToErrno(err)
		}
	}
	if i.upper != nil {
		if err := os.Remove(n.state.upperPath(p)); err != nil {
			if whiteout {
				_ = n.state.clearWhiteout(parent, name)
			}
			return fs.ToErrno(err)
		}
	}
	return 0
}

func (n *fuseNode) Unlink(_ context.Context, name string) syscall.Errno { return n.remove(name, false) }
func (n *fuseNode) Rmdir(_ context.Context, name string) syscall.Errno  { return n.remove(name, true) }

func (n *fuseNode) Rename(_ context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if !validName(name) || !validName(newName) {
		return syscall.EINVAL
	}
	to, ok := newParent.(*fuseNode)
	if !ok || to.state != n.state {
		return syscall.EXDEV
	}
	if flags != 0 {
		return syscall.EINVAL
	}
	n.state.mu.Lock()
	defer n.state.mu.Unlock()
	if !n.state.writable() {
		return syscall.EROFS
	}
	oldParent, newParentPath := n.path(), to.path()
	src, dst := joinPath(oldParent, name), joinPath(newParentPath, newName)
	if src == dst {
		return 0
	}
	i := n.state.stat(src)
	if !i.exists() {
		return syscall.ENOENT
	}
	if err := n.state.copyUp(src); err != nil {
		return fs.ToErrno(err)
	}
	if _, err := os.Lstat(n.state.upperPath(src)); err != nil {
		return syscall.EXDEV
	}
	if err := os.MkdirAll(n.state.upperPath(newParentPath), 0o755); err != nil {
		return fs.ToErrno(err)
	}
	sourceWhiteout := i.lower != nil
	if sourceWhiteout {
		if err := n.state.makeWhiteout(oldParent, name); err != nil {
			return fs.ToErrno(err)
		}
	}
	destWhiteout := n.state.hasWhiteout(newParentPath, newName)
	if destWhiteout {
		if err := n.state.clearWhiteout(newParentPath, newName); err != nil {
			if sourceWhiteout {
				_ = n.state.clearWhiteout(oldParent, name)
			}
			return fs.ToErrno(err)
		}
	}
	if err := os.Rename(n.state.upperPath(src), n.state.upperPath(dst)); err != nil {
		if sourceWhiteout {
			_ = n.state.clearWhiteout(oldParent, name)
		}
		if destWhiteout {
			_ = n.state.makeWhiteout(newParentPath, newName)
		}
		return fs.ToErrno(err)
	}
	return 0
}

var (
	_ fs.NodeLookuper   = (*fuseNode)(nil)
	_ fs.NodeGetattrer  = (*fuseNode)(nil)
	_ fs.NodeReaddirer  = (*fuseNode)(nil)
	_ fs.NodeReadlinker = (*fuseNode)(nil)
	_ fs.NodeOpener     = (*fuseNode)(nil)
	_ fs.NodeReader     = (*fuseNode)(nil)
	_ fs.NodeSetattrer  = (*fuseNode)(nil)
	_ fs.NodeCreater    = (*fuseNode)(nil)
	_ fs.NodeMkdirer    = (*fuseNode)(nil)
	_ fs.NodeUnlinker   = (*fuseNode)(nil)
	_ fs.NodeRmdirer    = (*fuseNode)(nil)
	_ fs.NodeRenamer    = (*fuseNode)(nil)
)
