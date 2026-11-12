package engine

import (
	"fmt"
	"testing"

	core "github.com/fvs-lab/core"
	fvsrepo "github.com/fvs-lab/fvs2/repo"
)

func TestLayerPrecedenceRootWhiteoutAndTypeReplacement(t *testing.T) {
	blocksPath := t.TempDir()
	store, err := core.NewDiskBlockStore(blocksPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Put([]byte("base"))
	if err != nil {
		t.Fatal(err)
	}
	base := Layer{BlockSize: 4, Files: []fvsrepo.FileEntry{
		{Path: "file", Size: 4, Blocks: []core.BlockID{id}},
		{Path: "dir/old", Size: 4, Blocks: []core.BlockID{id}},
		{Path: "opaque/old", Size: 4, Blocks: []core.BlockID{id}},
		{Path: "gone", Size: 4, Blocks: []core.BlockID{id}},
	}}
	higher := Layer{BlockSize: 8192, Files: []fvsrepo.FileEntry{
		{Path: ".wh.gone"},
		{Path: "dir"},
		{Path: "file/child"},
		{Path: "opaque/.wh..wh..opq"},
		{Path: "opaque/new"},
	}}
	tree, err := BuildMergedTreeFromRepos([]Layer{base, higher}, blocksPath)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(tree, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, p := range []string{"gone", "dir/old", "opaque/old"} {
		if _, err := e.Lookup(p); err == nil {
			t.Fatalf("lower visible: %s", p)
		}
	}
	for _, p := range []string{"dir", "file/child", "opaque/new"} {
		if _, err := e.Lookup(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
	// A file with a different layer's fixed block size still reads correctly.
	tree, err = BuildMergedTreeFromRepos([]Layer{base, {BlockSize: 8192}}, blocksPath)
	if err != nil {
		t.Fatal(err)
	}
	e, err = New(tree, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	h, err := e.Open("file", false)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if _, err := h.ReadAt(data, 0); err != nil || string(data) != "base" {
		t.Fatalf("mixed block sizes: %q %v", data, err)
	}
}
func TestOpaqueRootHidesLowerEntries(t *testing.T) {
	layer := Layer{Files: []fvsrepo.FileEntry{{Path: "old"}}}
	upper := Layer{Files: []fvsrepo.FileEntry{{Path: ".wh..wh..opq"}, {Path: "new"}}}
	tree, err := BuildMergedTreeFromRepos([]Layer{layer, upper}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if tree.Lookup(1, "old") != 0 || tree.Lookup(1, "new") == 0 {
		t.Fatal("root opacity")
	}
}
func BenchmarkMountMetadata10000(b *testing.B) {
	layer := Layer{Blocks: b.TempDir()}
	for i := 0; i < 10000; i++ {
		layer.Files = append(layer.Files, fvsrepo.FileEntry{Path: fmt.Sprintf("dir/file-%05d", i)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildMergedTreeFromRepos([]Layer{layer}, ""); err != nil {
			b.Fatal(err)
		}
	}
}
