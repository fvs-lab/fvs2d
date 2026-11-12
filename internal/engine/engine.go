package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type Engine struct {
	lower               *Tree
	upper               string
	root                *os.Root
	upperLock           *os.File
	closed              bool
	mu                  sync.Mutex
	uid, gid            uint32
	mtime               uint64
	clearPrivilegedBits bool
	initError           error
	ids                 map[string]uint64
	nextID              uint64
	handles             map[*Handle]struct{}
}

type Attr struct {
	ID      uint64 `json:"id"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	MtimeNS int64  `json:"mtime_ns"`
	Name    string `json:"name,omitempty"`
}

type Changes struct {
	Mode    *uint32 `json:"mode,omitempty"`
	Size    *int64  `json:"size,omitempty"`
	UID     *int    `json:"uid,omitempty"`
	GID     *int    `json:"gid,omitempty"`
	AtimeNS *int64  `json:"atime_ns,omitempty"`
	MtimeNS *int64  `json:"mtime_ns,omitempty"`
}

func New(lower *Tree, upper string, clearPrivilegedBits bool) (*Engine, error) {
	e := &Engine{lower: lower, upper: upper, uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), mtime: uint64(time.Now().Unix()), clearPrivilegedBits: clearPrivilegedBits, ids: map[string]uint64{"": 1}, nextID: uint64(len(lower.Nodes)) + 2, handles: map[*Handle]struct{}{}}
	if upper != "" {
		if err := os.MkdirAll(upper, 0o755); err != nil {
			return nil, err
		}
		r, err := os.OpenRoot(upper)
		if err != nil {
			return nil, err
		}
		e.root = r
		lock, err := r.OpenFile(".wh..wh..fvs-lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			r.Close()
			return nil, err
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			lock.Close()
			r.Close()
			return nil, fmt.Errorf("upper is already in use by another mount: %w", syscall.EBUSY)
		}
		e.upperLock = lock

	}
	return e, nil
}

func (s *Engine) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var failures []error
	for h := range s.handles {
		if err := h.close(); err != nil {
			failures = append(failures, err)
		}
		delete(s.handles, h)
	}
	if s.upperLock != nil {
		if err := s.upperLock.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	if s.root != nil {
		if err := s.root.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") && !strings.HasPrefix(name, WhiteoutPrefix)
}

func validPath(p string) bool {
	if p == "" {
		return true
	}
	if path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if !validName(part) {
			return false
		}
	}
	return true
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
func splitPath(p string) (string, string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
func rooted(p string) string {
	if p == "" {
		return "."
	}
	return p
}
func (s *Engine) writable() bool            { return s.root != nil }
func (s *Engine) upperPath(p string) string { return filepath.Join(s.upper, filepath.FromSlash(p)) }
func (s *Engine) lowerNode(p string) *Node {
	if p == "" {
		return s.lower.Get(1)
	}
	cur := uint64(1)
	for _, part := range strings.Split(p, "/") {
		cur = s.lower.Lookup(cur, part)
		if cur == 0 {
			return nil
		}
	}
	return s.lower.Get(cur)
}

type pathInfo struct {
	lower *Node
	upper *syscall.Stat_t
	info  os.FileInfo
}

func (i pathInfo) exists() bool { return i.lower != nil || i.upper != nil }
func (i pathInfo) Mode() uint32 {
	if i.upper != nil {
		return uint32(i.upper.Mode)
	}
	if i.lower == nil {
		return 0
	}
	switch {
	case i.lower.IsDir:
		return syscall.S_IFDIR | i.lower.Mode&0o7777
	case i.lower.Kind == "symlink" || i.lower.Link != "":
		return syscall.S_IFLNK | 0o777
	case i.lower.Kind == "fifo":
		return syscall.S_IFIFO | i.lower.Mode&0o7777
	default:
		return syscall.S_IFREG | i.lower.Mode&0o7777
	}
}
func (i pathInfo) IsDir() bool  { return i.Mode()&syscall.S_IFMT == syscall.S_IFDIR }
func (i pathInfo) isLink() bool { return i.Mode()&syscall.S_IFMT == syscall.S_IFLNK }
func (s *Engine) Mode(i pathInfo) uint32 {
	mode := i.Mode()
	if s.clearPrivilegedBits {
		mode &^= syscall.S_ISUID | syscall.S_ISGID
	}
	return mode
}
func (s *Engine) marker(p string) (bool, error) {
	if s.root == nil {
		return false, nil
	}
	_, err := s.root.Lstat(rooted(p))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err == nil {
		if spellingErr := s.checkUpperSpelling(p); errors.Is(spellingErr, syscall.EILSEQ) {
			return false, nil
		} else if spellingErr != nil {
			return false, spellingErr
		}
	}
	return err == nil, err
}
func (s *Engine) hasWhiteout(parent, base string) bool {
	found, _ := s.marker(joinPath(parent, WhiteoutPrefix+base))
	return found
}

// inspect rejects symlinks in parents. The upper root also confines races with
// external changes to the upper directory, including symlink replacement.
func (s *Engine) inspect(p string) (pathInfo, error) {
	if s.closed {
		return pathInfo{}, syscall.EBADF
	}
	if s.initError != nil {
		return pathInfo{}, s.initError
	}
	if !validPath(p) {
		return pathInfo{}, syscall.EINVAL
	}
	lowerVisible := true
	parts := strings.Split(p, "/")
	parent := ""
	for index, part := range parts {
		if p == "" {
			break
		}
		hidden, err := s.marker(joinPath(parent, WhiteoutPrefix+part))
		if err != nil {
			return pathInfo{}, err
		}
		if hidden {
			return pathInfo{}, syscall.ENOENT
		}
		opaque, err := s.marker(joinPath(parent, ".wh..wh..opq"))
		if err != nil {
			return pathInfo{}, err
		}
		if opaque {
			lowerVisible = false
		}
		if index < len(parts)-1 {
			prefix := joinPath(parent, part)
			info, err := s.rawStat(prefix, lowerVisible)
			if err != nil {
				return pathInfo{}, err
			}
			if !info.IsDir() {
				return pathInfo{}, syscall.ENOTDIR
			}
			parent = prefix
		}
	}
	return s.rawStat(p, lowerVisible)
}
func (s *Engine) rawStat(p string, lowerVisible bool) (pathInfo, error) {
	var lower *Node
	if lowerVisible {
		lower = s.lowerNode(p)
	}
	if s.root != nil {
		info, err := s.root.Lstat(rooted(p))
		if err == nil {
			if err := s.checkUpperSpelling(p); err != nil {
				return pathInfo{}, err
			}
			return pathInfo{lower: lower, upper: info.Sys().(*syscall.Stat_t), info: info}, nil
		}
		if !os.IsNotExist(err) {
			return pathInfo{}, err
		}
	}
	if lower == nil {
		return pathInfo{}, syscall.ENOENT
	}
	return pathInfo{lower: lower}, nil
}
func (s *Engine) stat(p string) pathInfo { i, _ := s.inspect(p); return i }
func (s *Engine) attr(p string, i pathInfo) Attr {
	id := s.ids[p]
	if id == 0 {
		if i.lower != nil {
			id = i.lower.Ino
		} else {
			id = s.nextID
			s.nextID++
		}
		s.ids[p] = id
	}
	a := Attr{ID: id, Mode: s.Mode(i), UID: s.uid, GID: s.gid, MtimeNS: int64(s.mtime) * 1e9}
	_, a.Name = splitPath(p)
	if i.lower != nil {
		a.Size = i.lower.Size
		a.MtimeNS = i.lower.MtimeNS
	}
	if i.upper != nil {
		a.Size = i.info.Size()
		a.MtimeNS = i.info.ModTime().UnixNano()
		a.UID = i.upper.Uid
		a.GID = i.upper.Gid
	}
	return a
}
func (s *Engine) Lookup(p string) (Attr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.inspect(p)
	if err != nil {
		return Attr{}, err
	}
	return s.attr(p, i), nil
}
func (s *Engine) names(p string) ([]string, error) {
	i, err := s.inspect(p)
	if err != nil {
		return nil, err
	}
	if !i.IsDir() {
		return nil, syscall.ENOTDIR
	}
	names := map[string]bool{}
	opaque, err := s.marker(joinPath(p, ".wh..wh..opq"))
	if err != nil {
		return nil, err
	}
	if i.lower != nil && i.lower.IsDir && !opaque {
		for name := range i.lower.Children {
			names[name] = true
		}
	}
	if s.root != nil {
		dir, err := s.root.Open(rooted(p))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			entries, readErr := dir.ReadDir(-1)
			closeErr := dir.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			for _, entry := range entries {
				name := entry.Name()
				if !strings.HasPrefix(name, WhiteoutPrefix) {
					names[name] = true
				}
			}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		if _, err := s.inspect(joinPath(p, name)); err == nil {
			out = append(out, name)
		} else if !errors.Is(err, syscall.ENOENT) {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}
func (s *Engine) ReadDir(p string) ([]Attr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.names(p)
	if err != nil {
		return nil, err
	}
	out := make([]Attr, 0, len(names))
	for _, name := range names {
		child := joinPath(p, name)
		i, err := s.inspect(child)
		if err != nil {
			return nil, err
		}
		out = append(out, s.attr(child, i))
	}
	return out, nil
}
func (s *Engine) Readlink(p string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.inspect(p)
	if err != nil {
		return "", err
	}
	if !i.isLink() {
		return "", syscall.EINVAL
	}
	if i.upper != nil {
		return s.root.Readlink(p)
	}
	return i.lower.Link, nil
}
func (s *Engine) syncDir(p string) error {
	f, err := s.root.Open(rooted(p))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Engine) ensureParents(p string) error {
	parent, _ := splitPath(p)
	if parent == "" {
		return nil
	}
	if err := s.ensureParents(parent); err != nil {
		return err
	}
	i, err := s.inspect(parent)
	if err != nil {
		return err
	}
	if !i.IsDir() {
		return syscall.ENOTDIR
	}
	if i.upper == nil {
		if err := s.root.Mkdir(parent, 0o700); err != nil {
			return err
		}
		if err := s.root.Chmod(parent, permissions(s.Mode(i))); err != nil {
			return err
		}
		par, _ := splitPath(parent)
		return s.syncDir(par)
	}
	return nil
}
func permissions(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}
func (s *Engine) copyUp(p string) error {
	i, err := s.inspect(p)
	if err != nil {
		return err
	}
	if i.upper != nil {
		return nil
	}
	if !s.writable() {
		return syscall.EROFS
	}
	if err := s.ensureParents(p); err != nil {
		return err
	}
	lower := i.lower
	parent, _ := splitPath(p)
	if i.IsDir() {
		if err := s.root.Mkdir(rooted(p), 0o700); err != nil {
			return err
		}
		if err := s.root.Chmod(rooted(p), permissions(s.Mode(i))); err != nil {
			return err
		}
		return s.syncDir(parent)
	}
	if i.isLink() {
		if err := s.root.Symlink(lower.Link, p); err != nil {
			return err
		}
		return s.syncDir(parent)
	}
	if i.Mode()&syscall.S_IFMT != syscall.S_IFREG {
		return syscall.ENOTSUP
	}
	// The temporary name is hidden by the whiteout namespace after a crash.
	var f *os.File
	var temp string
	for attempt := 0; attempt < 100; attempt++ {
		temp = joinPath(parent, fmt.Sprintf(".wh..wh..fvs-copy-%d-%d", os.Getpid(), time.Now().UnixNano()))
		f, err = s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if !os.IsExist(err) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() { f.Close(); s.root.Remove(temp) }()
	for off := int64(0); off < lower.Size; {
		data, err := s.lower.ReadAt(lower, off, 1<<20)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return io.ErrUnexpectedEOF
		}
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		off += int64(n)
	}
	if err := f.Chmod(permissions(s.Mode(i))); err != nil {
		return err
	}
	t := time.Unix(0, lower.MtimeNS)
	if err := s.root.Chtimes(temp, t, t); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := s.root.Rename(temp, p); err != nil {
		return err
	}
	if err := s.syncDir(parent); err != nil {
		return err
	}
	for h := range s.handles {
		if h.name != p || h.deleted {
			continue
		}
		h.mu.Lock()
		if h.file == nil {
			h.file, err = s.root.Open(p)
		}
		h.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Engine) clearWhiteout(parent, base string) error {
	marker := joinPath(parent, WhiteoutPrefix+base)
	found, err := s.marker(marker)
	if err != nil || !found {
		return err
	}
	err = s.root.Remove(marker)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *Engine) makeWhiteout(parent, base string) error {
	if err := s.ensureParents(joinPath(parent, base)); err != nil {
		return err
	}
	marker := joinPath(parent, WhiteoutPrefix+base)
	f, err := s.root.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return s.checkUpperSpelling(marker)
	}
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return s.syncDir(parent)
}

func (s *Engine) Create(p, kind, target string, mode uint32) (Attr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(p, kind, target, mode)
}

func (s *Engine) CreateOpen(p string, mode uint32) (Attr, *Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.create(p, "file", "", 0o600)
	if err != nil {
		return Attr{}, nil, err
	}
	h, err := s.open(p, true)
	if err == nil {
		if s.clearPrivilegedBits {
			mode &^= 0o6000
		}
		err = h.file.Chmod(permissions(mode))
	}
	if err == nil {
		err = h.file.Sync()
	}
	if err != nil {
		if h != nil {
			h.close()
			delete(s.handles, h)
		}
		return Attr{}, nil, errors.Join(err, s.remove(p, false))
	}
	a.Mode = a.Mode&syscall.S_IFMT | mode&0o7777
	return a, h, nil
}

func (s *Engine) create(p, kind, target string, mode uint32) (Attr, error) {
	if !s.writable() {
		return Attr{}, syscall.EROFS
	}
	if p == "" || !validPath(p) {
		return Attr{}, syscall.EINVAL
	}
	if _, err := s.inspect(p); err == nil {
		return Attr{}, syscall.EEXIST
	} else if !errors.Is(err, syscall.ENOENT) {
		return Attr{}, err
	}
	if err := s.ensureParents(p); err != nil {
		return Attr{}, err
	}
	parent, base := splitPath(p)
	// A removed upper entry can remain behind its durable whiteout.
	if hidden, _ := s.marker(joinPath(parent, WhiteoutPrefix+base)); hidden {
		if err := s.root.Remove(p); err != nil && !os.IsNotExist(err) {
			return Attr{}, err
		}
	}
	var err error
	if s.clearPrivilegedBits {
		mode &^= 0o6000
	}
	switch kind {
	case "dir":
		err = s.root.Mkdir(p, 0o700)
		if err == nil {
			err = s.root.Chmod(p, permissions(mode))
		}
	case "symlink":
		if strings.ContainsRune(target, 0) || target == "" {
			return Attr{}, syscall.EINVAL
		}
		err = s.root.Symlink(target, p)
	case "file":
		var f *os.File
		f, err = s.root.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			err = f.Chmod(permissions(mode))
			if err == nil {
				err = f.Sync()
			}
			cerr := f.Close()
			if err == nil {
				err = cerr
			}
		}
	default:
		return Attr{}, syscall.ENOTSUP
	}
	if err != nil {
		return Attr{}, err
	}
	if kind == "dir" && s.lowerNode(p) != nil {
		if err := s.makeOpaque(p); err != nil {
			return Attr{}, err
		}
	}

	if err := s.clearWhiteout(parent, base); err != nil {
		return Attr{}, err
	}
	if err := s.syncDir(parent); err != nil {
		return Attr{}, err
	}
	s.ids[p] = s.nextID
	s.nextID++
	i, err := s.inspect(p)
	if err != nil {
		return Attr{}, err
	}
	return s.attr(p, i), nil
}
func (s *Engine) Remove(p string, dir bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remove(p, dir)
}
func (s *Engine) remove(p string, dir bool) error {
	if !s.writable() {
		return syscall.EROFS
	}
	if p == "" {
		return syscall.EBUSY
	}
	i, err := s.inspect(p)
	if err != nil {
		return err
	}
	if dir != i.IsDir() {
		if dir {
			return syscall.ENOTDIR
		}
		return syscall.EISDIR
	}
	if dir {
		names, err := s.names(p)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return syscall.ENOTEMPTY
		}
	}
	parent, base := splitPath(p)
	if s.lowerNode(p) != nil {
		if err := s.makeWhiteout(parent, base); err != nil {
			return err
		}
	}
	if i.upper != nil {
		if dir {
			if err := s.clearDirectoryMarkers(p); err != nil {
				return err
			}
		}

		if err := s.root.Remove(p); err != nil {
			return err
		}
	}
	for h := range s.handles {
		if h.name == p {
			h.deleted = true
		}
	}
	delete(s.ids, p)
	return s.syncDir(parent)
}
func (s *Engine) materialize(p string) (result error) {
	if err := s.copyUp(p); err != nil {
		return err
	}
	i, err := s.inspect(p)
	if err != nil {
		return err
	}
	if !i.IsDir() {
		return nil
	}
	mode := permissions(s.Mode(i))
	if err := s.root.Chmod(rooted(p), mode|0o700); err != nil {
		return err
	}
	defer func() {
		if err := s.root.Chmod(rooted(p), mode); result == nil {
			result = err
		}
	}()
	names, err := s.names(p)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := s.materialize(joinPath(p, name)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Engine) Rename(src, dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writable() {
		return syscall.EROFS
	}
	if src == "" || dst == "" {
		return syscall.EBUSY
	}
	if !validPath(src) || !validPath(dst) {
		return syscall.EINVAL
	}
	i, err := s.inspect(src)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if strings.HasPrefix(dst, src+"/") {
		return syscall.EINVAL
	}
	target, terr := s.inspect(dst)
	if terr != nil && !errors.Is(terr, syscall.ENOENT) {
		return terr
	}
	if terr == nil {
		if i.IsDir() != target.IsDir() {
			if i.IsDir() {
				return syscall.ENOTDIR
			}
			return syscall.EISDIR
		}
		if target.IsDir() {
			names, err := s.names(dst)
			if err != nil {
				return err
			}
			if len(names) > 0 {
				return syscall.ENOTEMPTY
			}
		}
	}
	if err := s.materialize(src); err != nil {
		return err
	}
	if err := s.ensureParents(dst); err != nil {
		return err
	}
	sp, sb := splitPath(src)
	dp, db := splitPath(dst)
	if i.IsDir() {
		if err := s.makeOpaque(src); err != nil {
			return err
		}
	}

	sourceMarker := s.lowerNode(src) != nil
	if sourceMarker {
		if err := s.makeWhiteout(sp, sb); err != nil {
			return err
		}
	}
	if terr == nil && target.IsDir() && target.upper != nil {
		if err := s.clearDirectoryMarkers(dst); err != nil {
			return err
		}
	}

	if err := s.root.Rename(src, dst); err != nil {
		if sourceMarker {
			s.clearWhiteout(sp, sb)
		}
		return err
	}
	if err := s.clearWhiteout(dp, db); err != nil {
		return err
	}
	for h := range s.handles {
		if h.name == dst || strings.HasPrefix(h.name, dst+"/") {
			h.deleted = true
		}
		if h.name == src || strings.HasPrefix(h.name, src+"/") {
			h.name = dst + strings.TrimPrefix(h.name, src)
		}
	}
	moved := map[string]uint64{}
	for p, id := range s.ids {
		if p == dst || strings.HasPrefix(p, dst+"/") {
			delete(s.ids, p)
		}
		if p == src || strings.HasPrefix(p, src+"/") {
			moved[dst+strings.TrimPrefix(p, src)] = id
			delete(s.ids, p)
		}
	}
	for p, id := range moved {
		s.ids[p] = id
	}
	if err := s.syncDir(sp); err != nil {
		return err
	}
	return s.syncDir(dp)
}

func (s *Engine) Setattr(p string, c Changes) (Attr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setattr(p, c)
}
func (s *Engine) setattr(p string, c Changes) (Attr, error) {
	if !s.writable() {
		return Attr{}, syscall.EROFS
	}
	i, err := s.inspect(p)
	if err != nil {
		return Attr{}, err
	}
	if i.isLink() && c.Mode != nil && *c.Mode != 0o777 {
		return Attr{}, syscall.ENOTSUP
	}
	if c.Size != nil && (*c.Size < 0 || i.IsDir() || i.isLink()) {
		return Attr{}, syscall.EINVAL
	}
	if err := s.copyUp(p); err != nil {
		return Attr{}, err
	}
	if c.Mode != nil && !i.isLink() {
		mode := *c.Mode
		if s.clearPrivilegedBits {
			mode &^= 0o6000
		}
		if err := s.root.Chmod(rooted(p), permissions(mode)); err != nil {
			return Attr{}, err
		}
	}
	if c.Size != nil {
		f, err := s.root.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return Attr{}, err
		}
		err = f.Truncate(*c.Size)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return Attr{}, err
		}
	}
	if c.AtimeNS != nil || c.MtimeNS != nil {
		parent, base := splitPath(p)
		if p == "" {
			base = "."
		}
		dir, err := s.root.Open(rooted(parent))
		if err != nil {
			return Attr{}, err
		}
		times := []unix.Timespec{{Nsec: utimeOmit}, {Nsec: utimeOmit}}
		if c.AtimeNS != nil {
			times[0] = unix.NsecToTimespec(*c.AtimeNS)
		}
		if c.MtimeNS != nil {
			times[1] = unix.NsecToTimespec(*c.MtimeNS)
		}
		err = unix.UtimesNanoAt(int(dir.Fd()), base, times, unix.AT_SYMLINK_NOFOLLOW)
		dir.Close()
		if err != nil {
			return Attr{}, err
		}
	}

	if c.UID != nil || c.GID != nil {
		uid, gid := -1, -1
		if c.UID != nil {
			uid = *c.UID
		}
		if c.GID != nil {
			gid = *c.GID
		}
		if err := s.root.Lchown(rooted(p), uid, gid); err != nil {
			return Attr{}, err
		}
	}
	i, err = s.inspect(p)
	if err != nil {
		return Attr{}, err
	}
	return s.attr(p, i), nil
}

// ValidateDarwinNames is a metadata-only pass. FSKit uses UTF-8 names and the
// writable upper must preserve byte-distinct names rather than aliasing them.
func (s *Engine) ValidateDarwinNames() error {
	for _, n := range s.lower.Nodes {
		if !utf8.ValidString(n.Name) || !utf8.ValidString(n.Link) {
			return fmt.Errorf("FSKit cannot represent non-UTF-8 name or symlink: %w", syscall.EILSEQ)
		}
		if len(n.Name) > 255 {
			return syscall.ENAMETOOLONG
		}
		if n.Kind == "fifo" {
			return fmt.Errorf("FSKit adapter does not support FIFO entries: %w", syscall.ENOTSUP)
		}
	}
	return s.validateUpperPlatform()
}
