package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	core "fvs-v2-core"
	fvsrepo "fvs2/repo"
	"fvs2d/fvs2dpb"
)

func put(t *testing.T, store *core.MemBlockStore, b string) core.BlockID {
	t.Helper()
	id, err := store.Put([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDiffUpperDirReportsOnlyRealChanges(t *testing.T) {
	store := core.NewMemBlockStore()
	tree := buildTree(store, 4096, []fvsrepo.FileEntry{
		{Path: "keep.txt", Mode: 0o644, Size: 4, Blocks: []core.BlockID{put(t, store, "same")}},
		{Path: "mod.txt", Mode: 0o644, Size: 3, Blocks: []core.BlockID{put(t, store, "old")}},
		{Path: "gone.txt", Mode: 0o644, Size: 6, Blocks: []core.BlockID{put(t, store, "byebye")}},
		{Path: "link", Link: "target"},
	})

	upper := t.TempDir()
	if err := os.WriteFile(filepath.Join(upper, "keep.txt"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "mod.txt"), []byte("new-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "added.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, whiteoutPrefix+"gone.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(upper, "link")); err != nil {
		t.Fatal(err)
	}

	var changes []*fvs2dpb.FileChange
	if err := diffUpperDir(tree, upper, "", &changes); err != nil {
		t.Fatal(err)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	got := map[string]fvs2dpb.ChangeKind{}
	for _, c := range changes {
		got[c.Path] = c.Kind
	}
	want := map[string]fvs2dpb.ChangeKind{
		"added.txt": fvs2dpb.ChangeKind_CHANGE_KIND_ADDED,
		"mod.txt":   fvs2dpb.ChangeKind_CHANGE_KIND_MODIFIED,
		"gone.txt":  fvs2dpb.ChangeKind_CHANGE_KIND_REMOVED,
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Fatalf("path %q kind = %v, want %v (all: %+v)", p, got[p], k, got)
		}
	}
}
