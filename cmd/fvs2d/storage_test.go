package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	fvsrepo "github.com/fvs-lab/fvs2/repo"
)

func TestMergedTreeMemory(t *testing.T) {
	paths := filepath.SplitList(os.Getenv("FVS2D_BENCHMARK_LAYERS"))
	if len(paths) == 0 {
		t.Skip("FVS2D_BENCHMARK_LAYERS is not set")
	}
	layers := make([]resolvedCommit, 0, len(paths))
	files := 0
	blocks := 0
	uniqueBlocks := map[string]struct{}{}
	for _, repository := range paths {
		layer, err := resolveLayer(layerSel{repo: repository})
		if err != nil {
			t.Fatal(err)
		}
		files += len(layer.files)
		for _, file := range layer.files {
			blocks += len(file.Blocks)
			for _, block := range file.Blocks {
				uniqueBlocks[string(block)] = struct{}{}
			}
		}
		layers = append(layers, layer)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	tree, err := buildMergedTreeFromRepos(layers, "")
	if err != nil {
		t.Fatal(err)
	}
	layers = nil
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	inuse := int64(after.HeapInuse) - int64(before.HeapInuse)
	t.Logf("files=%d blocks=%d unique=%d nodes=%d heap=%d inuse=%d", files, blocks, len(uniqueBlocks), len(tree.nodes), heap, inuse)
}

func TestMergedTreeReadsSharedBlockStore(t *testing.T) {
	root := t.TempDir()
	blocks := filepath.Join(root, "blocks")
	content := []byte("shared")
	var layers []resolvedCommit
	for _, name := range []string{"lower", "upper"} {
		repository, err := fvsrepo.InitWithOptions(filepath.Join(root, name), fvsrepo.InitOptions{BlocksPath: blocks})
		if err != nil {
			t.Fatal(err)
		}
		writer, err := fvsrepo.BeginSnapshot(repository.Path, fvsrepo.SnapshotOptions{Message: name})
		if err != nil {
			t.Fatal(err)
		}
		path := "usr/share/" + name
		if err := writer.Add(fvsrepo.Entry{Path: path, Kind: fvsrepo.EntryFile, Mode: 0o644, Size: int64(len(content))}, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		layer, err := resolveLayer(layerSel{repo: repository.Path})
		if err != nil {
			t.Fatal(err)
		}
		if layer.blocks != blocks {
			t.Fatalf("blocks = %q, want %q", layer.blocks, blocks)
		}
		layers = append(layers, layer)
	}

	tree, err := buildMergedTreeFromRepos(layers, "")
	if err != nil {
		t.Fatal(err)
	}
	usr := tree.get(tree.lookup(1, "usr"))
	share := tree.get(tree.lookup(usr.ino, "share"))
	for _, name := range []string{"lower", "upper"} {
		node := tree.get(tree.lookup(share.ino, name))
		got, err := tree.readAt(node, 0, len(content))
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("read %s = %q, %v", name, got, err)
		}
	}
}

func TestMergedTreeAppliesDirectoryWhiteouts(t *testing.T) {
	lower := resolvedCommit{blockSize: 4096, files: []fvsrepo.FileEntry{
		{Path: "app/cache/a", Mode: 0o644},
		{Path: "app/cache/nested/b", Mode: 0o644},
	}}
	upper := resolvedCommit{blockSize: 4096, files: []fvsrepo.FileEntry{
		{Path: "app/.wh.cache", Mode: 0o644},
		{Path: "app/new", Mode: 0o644},
	}}
	tree, err := buildMergedTreeFromRepos([]resolvedCommit{lower, upper}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := tree.get(tree.lookup(1, "app"))
	if app == nil {
		t.Fatal("app directory is missing")
	}
	if tree.lookup(app.ino, "cache") != 0 {
		t.Fatal("whiteout left the lower directory visible")
	}
	if tree.lookup(app.ino, "new") == 0 {
		t.Fatal("upper file is missing")
	}
}

func TestMountManagerSharesIdenticalTrees(t *testing.T) {
	mgr := newMountManager(nil, nil)
	layers := []resolvedCommit{{blocks: t.TempDir(), blockSize: 4096}}
	key, first, err := mgr.acquireTree(layers)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := mgr.acquireTree(layers)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("identical mounts built separate trees")
	}
	mgr.releaseTree(key)
	if mgr.trees[key] == nil || mgr.trees[key].refs != 1 {
		t.Fatal("tree cache released too early")
	}
	mgr.releaseTree(key)
	if mgr.trees[key] != nil {
		t.Fatal("unused tree remains cached")
	}
}
