package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type overlay struct {
	lower *fsTree
	upper string

	inoToPath map[uint64]string
	pathToIno map[string]uint64
	next      uint64
}

func newOverlay(lower *fsTree, upper string) *overlay {
	return &overlay{
		lower:     lower,
		upper:     upper,
		inoToPath: map[uint64]string{1: ""},
		pathToIno: map[string]uint64{"": 1},
		next:      2,
	}
}

func (o *overlay) writable() bool { return o.upper != "" }

func (o *overlay) inoFor(p string) uint64 {
	if id, ok := o.pathToIno[p]; ok {
		return id
	}
	id := o.next
	o.next++
	o.inoToPath[id] = p
	o.pathToIno[p] = id
	return id
}

func (o *overlay) pathOf(ino uint64) (string, bool) {
	p, ok := o.inoToPath[ino]
	return p, ok
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func splitPath(p string) (parent, base string) {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func (o *overlay) upperJoin(p string) string { return filepath.Join(o.upper, p) }

func (o *overlay) hasWhiteout(parent, base string) bool {
	if !o.writable() {
		return false
	}
	_, err := os.Lstat(o.upperJoin(joinPath(parent, whiteoutPrefix+base)))
	return err == nil
}

func (o *overlay) lowerNode(p string) *fileNode {
	if o.lower == nil {
		return nil
	}
	if p == "" {
		return o.lower.get(1)
	}
	cur := uint64(1)
	for _, part := range strings.Split(p, "/") {
		nx := o.lower.lookup(cur, part)
		if nx == 0 {
			return nil
		}
		cur = nx
	}
	return o.lower.get(cur)
}

type statRes struct {
	exists, isDir, isLink, upper bool
	size                         int64
	mode                         uint32
}

func fullMode(s statRes) uint32 {
	switch {
	case s.isDir:
		return syscall.S_IFDIR | (s.mode & 0o7777)
	case s.isLink:
		return syscall.S_IFLNK | 0o777
	default:
		return syscall.S_IFREG | (s.mode & 0o7777)
	}
}

func (o *overlay) statPath(p string) statRes {
	if p == "" {
		return statRes{exists: true, isDir: true, mode: 0o755}
	}
	parent, base := splitPath(p)
	if o.writable() {
		if o.hasWhiteout(parent, base) {
			return statRes{}
		}
		if fi, err := os.Lstat(o.upperJoin(p)); err == nil {
			r := statRes{exists: true, upper: true, size: fi.Size(), mode: uint32(fi.Mode().Perm())}
			r.isDir = fi.IsDir()
			r.isLink = fi.Mode()&os.ModeSymlink != 0
			return r
		}
	}
	if ln := o.lowerNode(p); ln != nil {
		return statRes{exists: true, isDir: ln.isDir, isLink: ln.link != "", size: ln.size, mode: ln.mode & 0o7777}
	}
	return statRes{}
}

func (o *overlay) getattr(ino uint64) (mode uint32, size int64, nlink uint32, ok bool) {
	p, found := o.pathOf(ino)
	if !found {
		return 0, 0, 0, false
	}
	s := o.statPath(p)
	if !s.exists {
		return 0, 0, 0, false
	}
	nlink = 1
	if s.isDir {
		nlink = 2
	}
	return fullMode(s), s.size, nlink, true
}

func (o *overlay) lookup(parent uint64, name string) uint64 {
	pp, ok := o.pathOf(parent)
	if !ok {
		return 0
	}
	cp := joinPath(pp, name)
	if !o.statPath(cp).exists {
		return 0
	}
	return o.inoFor(cp)
}

type dirent struct {
	name string
	ino  uint64
	mode uint32
}

func (o *overlay) listDir(ino uint64) ([]dirent, bool) {
	p, ok := o.pathOf(ino)
	if !ok {
		return nil, false
	}
	s := o.statPath(p)
	if !s.exists || !s.isDir {
		return nil, false
	}
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if ln := o.lowerNode(p); ln != nil && ln.isDir {
		for n := range ln.children {
			if o.hasWhiteout(p, n) {
				continue
			}
			add(n)
		}
	}
	if o.writable() {
		if ents, err := os.ReadDir(o.upperJoin(p)); err == nil {
			for _, e := range ents {
				n := e.Name()
				if strings.HasPrefix(n, whiteoutPrefix) {
					continue
				}
				add(n)
			}
		}
	}
	sort.Strings(names)
	out := make([]dirent, 0, len(names))
	for _, n := range names {
		cs := o.statPath(joinPath(p, n))
		if !cs.exists {
			continue
		}
		out = append(out, dirent{name: n, ino: o.inoFor(joinPath(p, n)), mode: fullMode(cs)})
	}
	return out, true
}

func (o *overlay) readAt(ino uint64, off int64, n int) ([]byte, syscall.Errno) {
	p, ok := o.pathOf(ino)
	if !ok {
		return nil, syscall.ENOENT
	}
	s := o.statPath(p)
	if !s.exists {
		return nil, syscall.ENOENT
	}
	if s.isDir {
		return nil, syscall.EISDIR
	}
	if s.upper {
		data, err := readFileRegion(o.upperJoin(p), off, n)
		if err != nil {
			return nil, syscall.EIO
		}
		return data, 0
	}
	ln := o.lowerNode(p)
	if ln == nil {
		return nil, syscall.ENOENT
	}
	data, err := o.lower.readAt(ln, off, n)
	if err != nil {
		return nil, syscall.EIO
	}
	return data, 0
}

func (o *overlay) readlink(ino uint64) (string, bool) {
	p, ok := o.pathOf(ino)
	if !ok {
		return "", false
	}
	s := o.statPath(p)
	if !s.exists || !s.isLink {
		return "", false
	}
	if s.upper {
		tgt, err := os.Readlink(o.upperJoin(p))
		if err != nil {
			return "", false
		}
		return tgt, true
	}
	if ln := o.lowerNode(p); ln != nil {
		return ln.link, true
	}
	return "", false
}

func readFileRegion(p string, off int64, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:m], nil
}

func (o *overlay) copyUp(p string) error {
	up := o.upperJoin(p)
	if _, err := os.Lstat(up); err == nil {
		return nil
	}
	ln := o.lowerNode(p)
	if ln == nil || ln.isDir {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		return err
	}
	data := make([]byte, 0, ln.size)
	var off int64
	for off < ln.size {
		chunk, err := o.lower.readAt(ln, off, 1<<20)
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			break
		}
		data = append(data, chunk...)
		off += int64(len(chunk))
	}
	return os.WriteFile(up, data, os.FileMode(ln.mode&0o7777))
}

func (o *overlay) clearWhiteout(parent, base string) {
	if o.writable() {
		_ = os.Remove(o.upperJoin(joinPath(parent, whiteoutPrefix+base)))
	}
}

func (o *overlay) create(parent uint64, name string, mode uint32) (uint64, syscall.Errno) {
	if !o.writable() {
		return 0, syscall.EROFS
	}
	pp, ok := o.pathOf(parent)
	if !ok {
		return 0, syscall.ENOENT
	}
	if err := os.MkdirAll(o.upperJoin(pp), 0o755); err != nil {
		return 0, syscall.EIO
	}
	o.clearWhiteout(pp, name)
	cp := joinPath(pp, name)
	f, err := os.OpenFile(o.upperJoin(cp), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(mode&0o7777))
	if err != nil {
		return 0, syscall.EIO
	}
	_ = f.Close()
	return o.inoFor(cp), 0
}

func (o *overlay) write(ino uint64, data []byte, off int64) (int, syscall.Errno) {
	if !o.writable() {
		return 0, syscall.EROFS
	}
	p, ok := o.pathOf(ino)
	if !ok {
		return 0, syscall.ENOENT
	}
	if err := o.copyUp(p); err != nil {
		return 0, syscall.EIO
	}
	f, err := os.OpenFile(o.upperJoin(p), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return 0, syscall.EIO
	}
	defer f.Close()
	m, err := f.WriteAt(data, off)
	if err != nil {
		return 0, syscall.EIO
	}
	return m, 0
}

func (o *overlay) truncate(ino uint64, size int64) syscall.Errno {
	if !o.writable() {
		return syscall.EROFS
	}
	p, ok := o.pathOf(ino)
	if !ok {
		return syscall.ENOENT
	}
	if err := o.copyUp(p); err != nil {
		return syscall.EIO
	}
	if err := os.Truncate(o.upperJoin(p), size); err != nil {
		return syscall.EIO
	}
	return 0
}

func (o *overlay) mkdir(parent uint64, name string, mode uint32) (uint64, syscall.Errno) {
	if !o.writable() {
		return 0, syscall.EROFS
	}
	pp, ok := o.pathOf(parent)
	if !ok {
		return 0, syscall.ENOENT
	}
	if err := os.MkdirAll(o.upperJoin(pp), 0o755); err != nil {
		return 0, syscall.EIO
	}
	o.clearWhiteout(pp, name)
	cp := joinPath(pp, name)
	if err := os.Mkdir(o.upperJoin(cp), os.FileMode(mode&0o7777)); err != nil && !os.IsExist(err) {
		return 0, syscall.EIO
	}
	return o.inoFor(cp), 0
}

func (o *overlay) remove(parent uint64, name string) syscall.Errno {
	if !o.writable() {
		return syscall.EROFS
	}
	pp, ok := o.pathOf(parent)
	if !ok {
		return syscall.ENOENT
	}
	cp := joinPath(pp, name)
	if _, err := os.Lstat(o.upperJoin(cp)); err == nil {
		_ = os.RemoveAll(o.upperJoin(cp))
	}
	if o.lowerNode(cp) != nil {
		if err := os.MkdirAll(o.upperJoin(pp), 0o755); err != nil {
			return syscall.EIO
		}
		wh := o.upperJoin(joinPath(pp, whiteoutPrefix+name))
		if f, err := os.OpenFile(wh, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_ = f.Close()
		} else {
			return syscall.EIO
		}
	}
	return 0
}

func (o *overlay) rename(parent uint64, name string, newParent uint64, newName string) syscall.Errno {
	if !o.writable() {
		return syscall.EROFS
	}
	pp, ok := o.pathOf(parent)
	if !ok {
		return syscall.ENOENT
	}
	np, ok := o.pathOf(newParent)
	if !ok {
		return syscall.ENOENT
	}
	src := joinPath(pp, name)
	dst := joinPath(np, newName)
	if err := o.copyUp(src); err != nil {
		return syscall.EIO
	}
	srcUp := o.upperJoin(src)
	if _, err := os.Lstat(srcUp); err != nil {
		if o.lowerNode(src) != nil {
			return syscall.EXDEV
		}
		return syscall.ENOENT
	}
	if err := os.MkdirAll(o.upperJoin(np), 0o755); err != nil {
		return syscall.EIO
	}
	o.clearWhiteout(np, newName)
	if err := os.Rename(srcUp, o.upperJoin(dst)); err != nil {
		return syscall.EIO
	}
	if id, ok := o.pathToIno[src]; ok {
		delete(o.pathToIno, src)
		if oldDst, ok := o.pathToIno[dst]; ok && oldDst != id {
			delete(o.inoToPath, oldDst)
		}
		o.pathToIno[dst] = id
		o.inoToPath[id] = dst
	}
	if o.lowerNode(src) != nil {
		wh := o.upperJoin(joinPath(pp, whiteoutPrefix+name))
		if f, err := os.OpenFile(wh, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_ = f.Close()
		}
	}
	return 0
}
