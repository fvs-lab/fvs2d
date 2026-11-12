package engine

import (
	"io"
	"os"
	"sync"
	"syscall"
)

type Handle struct {
	engine   *Engine
	mu       sync.RWMutex
	name     string
	node     *Node
	file     *os.File
	writable bool
	deleted  bool
	closed   bool
	id       uint64
}

func (s *Engine) Open(p string, write bool) (*Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open(p, write)
}
func (s *Engine) OpenWriteOnly(p string) (*Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openAccess(p, true, true)
}
func (s *Engine) open(p string, write bool) (*Handle, error) {
	return s.openAccess(p, write, false)
}
func (s *Engine) openAccess(p string, write, writeOnly bool) (*Handle, error) {
	i, err := s.inspect(p)
	if err != nil {
		return nil, err
	}
	if i.IsDir() {
		return nil, syscall.EISDIR
	}
	if i.isLink() {
		return nil, syscall.ELOOP
	}
	if i.Mode()&syscall.S_IFMT != syscall.S_IFREG {
		return nil, syscall.ENOTSUP
	}
	if write && !s.writable() {
		return nil, syscall.EROFS
	}
	if write && i.upper == nil {
		if err := s.copyUp(p); err != nil {
			return nil, err
		}
		i, err = s.inspect(p)
		if err != nil {
			return nil, err
		}
	}
	h := &Handle{engine: s, name: p, node: i.lower, writable: write, id: s.attr(p, i).ID}
	if i.upper != nil {
		flags := os.O_RDONLY
		if write {
			flags = os.O_RDWR
			if writeOnly {
				flags = os.O_WRONLY
			}
		}
		h.file, err = s.root.OpenFile(p, flags|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
	}
	s.handles[h] = struct{}{}
	return h, nil
}
func (h *Handle) ReadAt(buf []byte, off int64) (int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return 0, syscall.EBADF
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if h.file != nil {
		return h.file.ReadAt(buf, off)
	}
	data, err := h.engine.lower.ReadAt(h.node, off, len(buf))
	if err != nil {
		return 0, err
	}
	n := copy(buf, data)
	if n < len(buf) {
		return n, io.EOF
	}
	return n, nil
}
func (h *Handle) WriteAt(buf []byte, off int64) (int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed || h.file == nil {
		return 0, syscall.EBADF
	}
	if !h.writable {
		return 0, syscall.EBADF
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	return h.file.WriteAt(buf, off)
}
func (h *Handle) Stat() (Attr, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return Attr{}, syscall.EBADF
	}
	if h.file != nil {
		info, err := h.file.Stat()
		if err != nil {
			return Attr{}, err
		}
		i := pathInfo{upper: info.Sys().(*syscall.Stat_t), info: info}
		a := Attr{ID: h.id, Mode: h.engine.Mode(i), Size: info.Size(), UID: i.upper.Uid, GID: i.upper.Gid, MtimeNS: info.ModTime().UnixNano()}
		return a, nil
	}
	n := h.node
	return Attr{ID: h.id, Mode: h.engine.Mode(pathInfo{lower: n}), Size: n.Size, UID: h.engine.uid, GID: h.engine.gid, MtimeNS: n.MtimeNS}, nil
}
func (h *Handle) Sync() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return syscall.EBADF
	}
	if h.file != nil {
		return h.file.Sync()
	}
	return nil
}
func (h *Handle) close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if h.file != nil {
		return h.file.Close()
	}
	return nil
}
func (h *Handle) Close() error {
	s := h.engine
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handles, h)
	return h.close()
}
func (s *Engine) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h := range s.handles {
		if err := h.Sync(); err != nil {
			return err
		}
	}
	if s.root != nil {
		return s.syncDir("")
	}
	return nil
}

func (h *Handle) Setattr(c Changes) (Attr, error) {
	s := h.engine
	s.mu.Lock()
	defer s.mu.Unlock()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return Attr{}, syscall.EBADF
	}
	if h.file == nil && !h.deleted {
		h.mu.Unlock()
		return s.setattr(h.name, c)
	}
	if h.file == nil {
		h.mu.Unlock()
		return Attr{}, syscall.EBADF
	}
	var err error
	if h.deleted && (c.AtimeNS != nil || c.MtimeNS != nil) {
		h.mu.Unlock()
		return Attr{}, syscall.ENOTSUP
	}
	if c.Size != nil && *c.Size < 0 {
		h.mu.Unlock()
		return Attr{}, syscall.EINVAL
	}
	if c.Mode != nil {
		mode := *c.Mode
		if s.clearPrivilegedBits {
			mode &^= 0o6000
		}
		err = h.file.Chmod(permissions(mode))
	}
	if err == nil && c.Size != nil {
		if !h.writable {
			err = syscall.EBADF
		} else {
			err = h.file.Truncate(*c.Size)
		}
	}
	if err == nil && (c.UID != nil || c.GID != nil) {
		uid, gid := -1, -1
		if c.UID != nil {
			uid = *c.UID
		}
		if c.GID != nil {
			gid = *c.GID
		}
		err = h.file.Chown(uid, gid)
	}
	if err == nil && (c.AtimeNS != nil || c.MtimeNS != nil) {
		_, err = s.setattr(h.name, Changes{AtimeNS: c.AtimeNS, MtimeNS: c.MtimeNS})
	}
	if err == nil {
		err = h.file.Sync()
	}
	h.mu.Unlock()
	if err != nil {
		return Attr{}, err
	}
	return h.Stat()
}
