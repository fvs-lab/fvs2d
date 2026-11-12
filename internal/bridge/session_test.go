package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	core "github.com/fvs-lab/core"
	fvsrepo "github.com/fvs-lab/fvs2/repo"
	"github.com/fvs-lab/fvs2d/internal/engine"
)

func session(t testing.TB, upper bool) *Session {
	t.Helper()
	store := core.NewMemBlockStore()
	data := make([]byte, 8192)
	for i := range data {
		data[i] = byte(i % 251)
	}
	id, err := store.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	tree := engine.BuildTree(store, 8192, []fvsrepo.FileEntry{{Path: "file", Mode: 0o640, Size: 8192, Blocks: []core.BlockID{id}}})
	path := ""
	if upper {
		path = t.TempDir()
	}
	e, err := engine.New(tree, path, false)
	if err != nil {
		t.Fatal(err)
	}
	s := New(e)
	t.Cleanup(func() { s.Close() })
	return s
}
func request(t testing.TB, s *Session, r Request) Response {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out Response
	if err := json.Unmarshal(s.Call(data), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestBridgeLifecycleAndErrors(t *testing.T) {
	s := session(t, true)
	for _, bad := range []string{"", " \n", `{"op":"lookup","extra":true}`, `{"op":"lookup"} {}`, `invalid`} {
		var out Response
		if err := json.Unmarshal(s.Call([]byte(bad)), &out); err != nil || out.Errno != int(syscall.EINVAL) {
			t.Fatalf("invalid request %q: %+v %v", bad, out, err)
		}
	}
	if Errno(io.EOF) == 0 {
		t.Fatal("metadata EOF must not be reported as success")
	}
	created := request(t, s, Request{Op: "create", Kind: "file", Path: "created", Mode: 0})
	if created.Errno != 0 || created.Handle == 0 || created.Attr.Mode&0o7777 != 0 {
		t.Fatalf("create: %+v", created)
	}
	if _, err := s.Write(created.Handle, []byte("created"), 0); err != nil {
		t.Fatal(err)
	}
	length := int64(3)
	if r := request(t, s, Request{Op: "setattr", ID: created.Handle, Changes: engine.Changes{Size: &length}}); r.Errno != 0 || r.Attr.Size != 3 {
		t.Fatal(r)
	}
	read := request(t, s, Request{Op: "open", Path: "file"})
	if read.Errno != 0 || read.Handle == 0 {
		t.Fatalf("open %+v", read)
	}
	out := make([]byte, 4)
	if n, err := s.Read(read.Handle, out, 0); err != nil || n != 4 {
		t.Fatalf("read %d %v", n, err)
	}
	if _, err := s.Write(read.Handle, []byte("x"), 0); !errors.Is(err, syscall.EBADF) {
		t.Fatal(err)
	}
	write := request(t, s, Request{Op: "open", Path: "file", Write: true})
	if write.Errno != 0 {
		t.Fatal(write)
	}
	if _, err := s.Write(write.Handle, []byte("UP"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(read.Handle, out[:2], 0); err != nil || string(out[:2]) != "UP" {
		t.Fatalf("copy-up %q %v", out, err)
	}
	if n, err := s.Read(read.Handle, out, 9000); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF %d %v", n, err)
	}
	if r := request(t, s, Request{Op: "rename", Path: "file", Target: "renamed"}); r.Errno != 0 {
		t.Fatal(r)
	}
	size := int64(3)
	if r := request(t, s, Request{Op: "setattr", Path: "renamed", Changes: engine.Changes{Size: &size}}); r.Errno != 0 || r.Attr.Size != 3 {
		t.Fatal(r)
	}
	if r := request(t, s, Request{Op: "stat", ID: read.Handle}); r.Errno != 0 || r.Attr.Size != 3 {
		t.Fatal(r)
	}
	if r := request(t, s, Request{Op: "remove", Path: "renamed", Kind: "file"}); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := request(t, s, Request{Op: "lookup", Path: "file"}); r.Errno != int(syscall.ENOENT) {
		t.Fatal(r)
	}
	if r := request(t, s, Request{Op: "sync"}); r.Errno != 0 {
		t.Fatal(r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if r := request(t, s, Request{Op: "open", Path: "file"}); r.Errno != int(syscall.EBADF) {
		t.Fatal(r)
	}
}
func BenchmarkBinaryRead4K(b *testing.B) {
	for _, upper := range []bool{false, true} {
		b.Run(fmt.Sprintf("upper=%v", upper), func(b *testing.B) {
			s := session(b, upper)
			r := request(b, s, Request{Op: "open", Path: "file", Write: upper})
			if r.Errno != 0 {
				b.Fatal(r)
			}
			buf := make([]byte, 4096)
			b.SetBytes(4096)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Read(r.Handle, buf, int64(i%4096)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
