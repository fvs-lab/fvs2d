package bridge

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"

	"github.com/fvs-lab/fvs2d/internal/engine"
)

type Request struct {
	Op        string         `json:"op"`
	Path      string         `json:"path"`
	Target    string         `json:"target,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Mode      uint32         `json:"mode,omitempty"`
	Write     bool           `json:"write,omitempty"`
	WriteOnly bool           `json:"write_only,omitempty"`
	ID        uint64         `json:"id,omitempty"`
	Changes   engine.Changes `json:"changes,omitempty"`
}
type Response struct {
	Errno      int                `json:"errno"`
	Message    string             `json:"message,omitempty"`
	Attr       *engine.Attr       `json:"attr,omitempty"`
	Entries    []engine.Attr      `json:"entries,omitempty"`
	Target     string             `json:"target,omitempty"`
	Handle     uint64             `json:"handle,omitempty"`
	Statistics *engine.Statistics `json:"statistics,omitempty"`
}
type Session struct {
	engine  *engine.Engine
	mu      sync.RWMutex
	handles map[uint64]*engine.Handle
	next    uint64
	closed  bool
}

func New(e *engine.Engine) *Session {
	return &Session{engine: e, handles: map[uint64]*engine.Handle{}, next: 1}
}
func Errno(err error) int {
	if err == nil {
		return 0
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		return int(code)
	}
	return int(syscall.EIO)
}
func (s *Session) Call(data []byte) []byte {
	var req Request
	var resp Response
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	err := d.Decode(&req)
	if err == nil {
		err = d.Decode(new(any))
		if errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = syscall.EINVAL
		}
	}
	if err != nil {
		resp.Errno = int(syscall.EINVAL)
		resp.Message = err.Error()
	} else if resp, err = s.call(req); err != nil {
		resp.Errno = Errno(err)
		resp.Message = err.Error()
	}
	encoded, _ := json.Marshal(resp)
	return encoded
}
func (s *Session) call(r Request) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Response
	var attr engine.Attr
	var err error
	if s.closed {
		return out, syscall.EBADF
	}
	switch r.Op {
	case "lookup":
		attr, err = s.engine.Lookup(r.Path)
		out.Attr = &attr
	case "stat":
		if h := s.handles[r.ID]; h != nil {
			attr, err = h.Stat()
			out.Attr = &attr
		} else {
			err = syscall.EBADF
		}
	case "readdir":
		out.Entries, err = s.engine.ReadDir(r.Path)
	case "readlink":
		out.Target, err = s.engine.Readlink(r.Path)
	case "create":
		if r.Kind == "file" {
			var h *engine.Handle
			attr, h, err = s.engine.CreateOpen(r.Path, r.Mode)
			if err == nil {
				out.Handle = s.next
				s.next++
				s.handles[out.Handle] = h
			}
		} else {
			attr, err = s.engine.Create(r.Path, r.Kind, r.Target, r.Mode)
		}
		out.Attr = &attr
	case "setattr":
		if r.ID != 0 {
			if handle := s.handles[r.ID]; handle != nil {
				attr, err = handle.Setattr(r.Changes)
			} else {
				err = syscall.EBADF
			}
		} else {
			attr, err = s.engine.Setattr(r.Path, r.Changes)
		}
		out.Attr = &attr
	case "remove":
		err = s.engine.Remove(r.Path, r.Kind == "dir")
	case "rename":
		err = s.engine.Rename(r.Path, r.Target)
	case "open":
		var h *engine.Handle
		if r.WriteOnly {
			if !r.Write {
				return out, syscall.EINVAL
			}
			h, err = s.engine.OpenWriteOnly(r.Path)
		} else {
			h, err = s.engine.Open(r.Path, r.Write)
		}
		if err == nil {
			out.Handle = s.next
			s.next++
			s.handles[out.Handle] = h
		}
	case "close":
		if h := s.handles[r.ID]; h != nil {
			err = h.Close()
			delete(s.handles, r.ID)
		} else {
			err = syscall.EBADF
		}
	case "sync":
		err = s.engine.Sync()
	case "statfs":
		var statistics engine.Statistics
		statistics, err = s.engine.Statfs()
		out.Statistics = &statistics
	default:
		err = syscall.ENOTSUP
	}
	return out, err
}
func (s *Session) Read(id uint64, dest []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.handles[id]
	if h == nil || s.closed {
		return 0, syscall.EBADF
	}
	return h.ReadAt(dest, off)
}
func (s *Session) Write(id uint64, data []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.handles[id]
	if h == nil || s.closed {
		return 0, syscall.EBADF
	}
	return h.WriteAt(data, off)
}
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.engine.Close()
}
