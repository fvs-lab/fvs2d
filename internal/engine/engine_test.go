package engine

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	core "github.com/fvs-lab/core"
	fvsrepo "github.com/fvs-lab/fvs2/repo"
)

type countingStore struct {
	BlockGetter
	reads int
}

func (s *countingStore) Get(id core.BlockID) ([]byte, error) { s.reads++; return s.BlockGetter.Get(id) }
func fixture(t *testing.T, upper bool) (*Engine, []byte, *countingStore) {
	t.Helper()
	store := memStore{}
	data := make([]byte, 4<<20)
	rand.New(rand.NewSource(17)).Read(data)
	var blocks []core.BlockID
	var sizes []int64
	for start := 0; start < len(data); {
		end := min(len(data), start+8191)
		blocks = append(blocks, store.put(data[start:end]))
		sizes = append(sizes, int64(end-start))
		start = end
	}
	counted := &countingStore{BlockGetter: store}
	tree := BuildTree(counted, 4096, []fvsrepo.FileEntry{
		{Path: "dir", Kind: "dir", Mode: 0o750},
		{Path: "dir/data", Kind: "file", Mode: 0o640, Size: int64(len(data)), Blocks: blocks, BlockSizes: sizes, ModTimeNS: 1234567890123456789},
		{Path: "empty", Kind: "file", Mode: 0},
		{Path: "link", Kind: "symlink", Mode: 0o777, Link: "dir/data"},
	})
	path := ""
	if upper {
		path = t.TempDir()
	}
	e, err := New(tree, path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, data, counted
}
func TestLazyVerifiedReads(t *testing.T) {
	e, data, store := fixture(t, false)
	if store.reads != 0 {
		t.Fatal("mount metadata fetched data blocks")
	}
	h, err := e.Open("dir/data", false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	out := make([]byte, 3)
	if n, err := h.ReadAt(out, 9000); err != nil || n != 3 || !bytes.Equal(out, data[9000:9003]) {
		t.Fatalf("partial: %q %d %v", out, n, err)
	}
	if store.reads != 1 {
		t.Fatalf("3 bytes fetched %d blocks", store.reads)
	}
	full := make([]byte, len(data))
	if _, err := h.ReadAt(full, 0); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(full) != sha256.Sum256(data) {
		t.Fatal("hash mismatch")
	}
	rng := rand.New(rand.NewSource(73))
	for i := 0; i < 300; i++ {
		off := rng.Intn(len(data) + 1000)
		length := rng.Intn(20000)
		buf := make([]byte, length)
		n, err := h.ReadAt(buf, int64(off))
		want := 0
		if off < len(data) {
			want = min(length, len(data)-off)
		}
		if n != want || (!errors.Is(err, io.EOF) && err != nil) {
			t.Fatalf("read %d %d: %d %v", off, length, n, err)
		}
		if n > 0 && !bytes.Equal(buf[:n], data[off:off+n]) {
			t.Fatal("random mismatch")
		}
	}
	if n, err := h.ReadAt(make([]byte, 8), math.MaxInt64-1); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF %d %v", n, err)
	}
	if _, err := h.ReadAt(out, -1); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("negative offset: %v", err)
	}
	a, err := e.Lookup("dir/data")
	if err != nil || a.MtimeNS != 1234567890123456789 || a.Mode&0o777 != 0o640 {
		t.Fatalf("attrs %+v %v", a, err)
	}
	if target, err := e.Readlink("link"); err != nil || target != "dir/data" {
		t.Fatalf("link %q %v", target, err)
	}
	empty, err := e.Open("empty", false)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := empty.ReadAt(out, 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("empty: %d %v", n, err)
	}
	if _, err := e.Create("new", "file", "", 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatal(err)
	}
}
func TestCorruptAndMissingBlocksFailReads(t *testing.T) {
	e, _, store := fixture(t, false)
	node := e.lowerNode("dir/data")
	memory := store.BlockGetter.(memStore)
	id := node.Blocks[0]
	memory[id] = []byte("corrupt")
	h, err := e.Open("dir/data", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, core.ErrBlockCorrupt) {
		t.Fatalf("corruption: %v", err)
	}
	delete(memory, id)
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, core.ErrBlockNotFound) {
		t.Fatalf("missing: %v", err)
	}
	memory[id] = []byte("short")
	node.Blocks[0] = core.ContentID(memory[id])
	memory[node.Blocks[0]] = memory[id]
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short block: %v", err)
	}
}
func TestCopyUpOpenHandlesRenameAndTruncate(t *testing.T) {
	e, data, _ := fixture(t, true)
	reader, err := e.Open("dir/data", false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := e.Open("dir/data", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte("UP"), 37); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := reader.ReadAt(buf, 37); err != nil || string(buf) != "UP" {
		t.Fatalf("reader after copy-up: %q %v", buf, err)
	}
	lower, err := e.lower.ReadAt(e.lowerNode("dir/data"), 37, 2)
	if err != nil || !bytes.Equal(lower, data[37:39]) {
		t.Fatal("lower modified")
	}
	if err := e.Rename("dir/data", "moved"); err != nil {
		t.Fatal(err)
	}
	size := int64(40)
	if _, err := e.Setattr("moved", Changes{Size: &size}); err != nil {
		t.Fatal(err)
	}
	size = 50000
	if _, err := e.Setattr("moved", Changes{Size: &size}); err != nil {
		t.Fatal(err)
	}
	tail := make([]byte, 300)
	if _, err := reader.ReadAt(tail, 42); err != nil || !bytes.Equal(tail, make([]byte, 300)) {
		t.Fatal("truncate growth not zero-filled")
	}
	if err := e.Remove("moved", false); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte("still open"), 0); err != nil {
		t.Fatal(err)
	}
	a, err := e.Create("moved", "file", "", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	old, err := reader.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == old.ID {
		t.Fatal("reused inode for open unlinked file")
	}
	for _, p := range []string{"dir/data"} {
		if _, err := e.Lookup(p); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("whiteout missing: %v", err)
		}
	}
}
func TestDirectoryRenameAndWhiteoutRecreation(t *testing.T) {
	e, _, _ := fixture(t, true)
	if _, err := e.Create("dir/new", "file", "", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Rename("dir", "renamed"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"renamed/data", "renamed/new"} {
		if _, err := e.Lookup(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Lookup("dir/data"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("old descendant visible: %v", err)
	}
	if _, err := e.Create("dir", "dir", "", 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := e.ReadDir("dir")
	if err != nil || len(entries) != 0 {
		t.Fatalf("recreated directory leaked lower: %+v %v", entries, err)
	}
	if err := e.Remove("dir", true); err != nil {
		t.Fatal(err)
	}
	if err := e.Rename("renamed", "renamed/sub"); !errors.Is(err, syscall.EINVAL) {
		t.Fatal(err)
	}
}
func TestUpperPathsAndErrors(t *testing.T) {
	e, _, _ := fixture(t, true)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.upper, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../data", "/data", "dir/../empty", "dir/\x00bad", ".wh.private"} {
		if _, err := e.Lookup(p); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("unsafe %q: %v", p, err)
		}
	}
	if _, err := e.Create("escape/data", "file", "", 0o600); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("symlink parent: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "data")); string(got) != "untouched" {
		t.Fatal("escaped upper")
	}
	if err := e.Remove("dir", true); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatal(err)
	}
	if err := e.Rename("missing", "missing"); !errors.Is(err, syscall.ENOENT) {
		t.Fatal(err)
	}
}
func TestConcurrentReads(t *testing.T) {
	e, data, store := fixture(t, true)
	e.lower.Store = store.BlockGetter
	h, err := e.Open("dir/data", false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 80; j++ {
				off := int64((i*7919 + j*733) % (len(data) - 4096))
				buf := make([]byte, 4096)
				if _, err := h.ReadAt(buf, off); err != nil || !bytes.Equal(buf, data[off:off+4096]) {
					t.Errorf("parallel read: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestWritableUpperHasOneOwner(t *testing.T) {
	e, _, _ := fixture(t, true)
	if duplicate, err := New(e.lower, e.upper, false); !errors.Is(err, syscall.EBUSY) {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatalf("second upper owner: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := New(e.lower, e.upper, false)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
func TestOpenUnlinkedFileCanTruncate(t *testing.T) {
	e, _, _ := fixture(t, true)
	h, err := e.Open("dir/data", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Remove("dir/data", false); err != nil {
		t.Fatal(err)
	}
	size := int64(7)
	attr, err := h.Setattr(Changes{Size: &size})
	if err != nil || attr.Size != 7 {
		t.Fatalf("unlinked truncate: %+v %v", attr, err)
	}
}

func TestCreateOpenKeepsAccessAfterPermissionChanges(t *testing.T) {
	e, _, _ := fixture(t, true)
	a, h, err := e.CreateOpen("new", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if a.Mode&0o7777 != 0 {
		t.Fatalf("permissions changed: %o", a.Mode)
	}
	if _, err := h.WriteAt([]byte("created"), 0); err != nil {
		t.Fatal(err)
	}
	size := int64(3)
	if a, err := h.Setattr(Changes{Size: &size}); err != nil || a.Size != 3 {
		t.Fatalf("truncate retained descriptor: %+v %v", a, err)
	}
}

func TestWriteOnlyUpperDoesNotRequireReadPermission(t *testing.T) {
	e, _, _ := fixture(t, true)
	if _, err := e.Create("write-only", "file", "", 0o200); err != nil {
		t.Fatal(err)
	}
	h, err := e.OpenWriteOnly("write-only")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteAt([]byte("write"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("write-only descriptor allowed read: %v", err)
	}
}
func TestSymlinkMetadataDoesNotTouchTarget(t *testing.T) {
	e, _, _ := fixture(t, true)
	mtime := int64(1234567890123)
	before, err := e.Lookup("dir/data")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Setattr("link", Changes{MtimeNS: &mtime}); err != nil {
		t.Fatal(err)
	}
	after, err := e.Lookup("dir/data")
	if err != nil || after.MtimeNS != before.MtimeNS {
		t.Fatalf("target time changed: %+v %v", after, err)
	}
	link, err := e.Lookup("link")
	if err != nil || link.MtimeNS != mtime {
		t.Fatalf("link time: %+v %v", link, err)
	}
}

func TestRenameReadOnlyLowerDirectoryPreservesPermissions(t *testing.T) {
	e, _, _ := fixture(t, true)
	e.lowerNode("dir").Mode = 0o555
	defer os.Chmod(e.upperPath("moved-dir"), 0o700)
	if err := e.Rename("dir", "moved-dir"); err != nil {
		t.Fatal(err)
	}
	attr, err := e.Lookup("moved-dir")
	if err != nil || attr.Mode&0o777 != 0o555 {
		t.Fatalf("directory metadata: %+v %v", attr, err)
	}
	if _, err := e.Lookup("moved-dir/data"); err != nil {
		t.Fatal(err)
	}
}
