package main

import (
	"context"
	"os"
	"testing"

	core "fvs-v2-core"
	fvsrepo "fvs2/repo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestFuseNodeCopyUpAndWhiteout(t *testing.T) {
	store := core.NewMemBlockStore()
	id, err := store.Put([]byte("lower"))
	if err != nil {
		t.Fatal(err)
	}
	tree := buildTree(store, 4096, []fvsrepo.FileEntry{{Path: "file", Mode: 0o644, Size: 5, Blocks: []core.BlockID{id}}})
	root := newFuseRoot(tree, t.TempDir())
	_ = fs.NewNodeFS(root, &fs.Options{RootStableAttr: &fs.StableAttr{Ino: 1}})

	child, errno := root.Lookup(context.Background(), "file", &fuse.EntryOut{})
	if errno != 0 || !root.AddChild("file", child, false) {
		t.Fatalf("lookup: inode=%v errno=%v", child, errno)
	}
	node := child.Operations().(*fuseNode)
	fh, _, errno := node.Open(context.Background(), uint32(os.O_RDWR))
	if errno != 0 {
		t.Fatalf("open: %v", errno)
	}
	if _, errno = fh.(fs.FileWriter).Write(context.Background(), []byte("UP"), 0); errno != 0 {
		t.Fatalf("write: %v", errno)
	}
	_ = fh.(fs.FileReleaser).Release(context.Background())
	if got, err := os.ReadFile(root.state.upperPath("file")); err != nil || string(got) != "UPwer" {
		t.Fatalf("copy-up = %q, %v", got, err)
	}
	if errno := root.Unlink(context.Background(), "file"); errno != 0 {
		t.Fatalf("unlink: %v", errno)
	}
	if root.state.stat("file").exists() {
		t.Fatal("whiteout did not hide lower file")
	}
}
