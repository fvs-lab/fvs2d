//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"io"
	"sync"
	"syscall"
	"unsafe"

	"github.com/fvs-lab/fvs2d/internal/bridge"
	"github.com/fvs-lab/fvs2d/internal/engine"
)

var registry = struct {
	sync.RWMutex
	next     uint64
	sessions map[uint64]*bridge.Session
}{next: 1, sessions: map[uint64]*bridge.Session{}}

//export fvs_open
func fvs_open(source *C.char, result *C.uint64_t) *C.char {
	if source == nil || result == nil {
		return C.CString(`{"errno":22,"message":"invalid bridge arguments"}`)
	}
	e, err := engine.LoadResource(C.GoString(source))
	if err != nil {
		r := bridge.Response{Errno: bridge.Errno(err), Message: err.Error()}
		return encodeResponse(r)
	}
	registry.Lock()
	id := registry.next
	registry.next++
	registry.sessions[id] = bridge.New(e)
	registry.Unlock()
	*result = C.uint64_t(id)
	return nil
}

//export fvs_call
func fvs_call(id C.uint64_t, request *C.char) *C.char {
	registry.RLock()
	defer registry.RUnlock()
	s := registry.sessions[uint64(id)]
	if s == nil || request == nil {
		return C.CString(`{"errno":9,"message":"closed bridge session"}`)
	}
	return C.CString(string(s.Call([]byte(C.GoString(request)))))
}

//export fvs_read
func fvs_read(id C.uint64_t, handle C.uint64_t, buffer unsafe.Pointer, length C.int64_t, offset C.int64_t, count *C.int64_t) C.int {
	registry.RLock()
	defer registry.RUnlock()
	s := registry.sessions[uint64(id)]
	if s == nil || count == nil {
		return C.int(syscall.EBADF)
	}
	if length < 0 || length > 64<<20 || (length > 0 && buffer == nil) {
		return C.int(syscall.EINVAL)
	}
	*count = 0
	n, err := s.Read(uint64(handle), unsafe.Slice((*byte)(buffer), int(length)), int64(offset))
	*count = C.int64_t(n)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return C.int(bridge.Errno(err))
}

//export fvs_write
func fvs_write(id C.uint64_t, handle C.uint64_t, buffer unsafe.Pointer, length C.int64_t, offset C.int64_t, count *C.int64_t) C.int {
	registry.RLock()
	defer registry.RUnlock()
	s := registry.sessions[uint64(id)]
	if s == nil || count == nil {
		return C.int(syscall.EBADF)
	}
	if length < 0 || length > 64<<20 || (length > 0 && buffer == nil) {
		return C.int(syscall.EINVAL)
	}
	*count = 0
	n, err := s.Write(uint64(handle), unsafe.Slice((*byte)(buffer), int(length)), int64(offset))
	*count = C.int64_t(n)
	return C.int(bridge.Errno(err))
}

//export fvs_close
func fvs_close(id C.uint64_t) C.int {
	registry.Lock()
	defer registry.Unlock()
	s := registry.sessions[uint64(id)]
	if s == nil {
		return C.int(syscall.EBADF)
	}
	delete(registry.sessions, uint64(id))
	return C.int(bridge.Errno(s.Close()))
}

func main() {}
