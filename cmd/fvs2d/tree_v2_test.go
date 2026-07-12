package main

import (
	"bytes"
	"testing"

	core "fvs-v2-core"
	fvsrepo "fvs2/repo"
)

type memStore map[core.BlockID][]byte

func (m memStore) Get(id core.BlockID) ([]byte, error) {
	b, ok := m[id]
	if !ok {
		return nil, core.ErrBlockNotFound
	}
	return b, nil
}

func (m memStore) put(data []byte) core.BlockID {
	id := core.ContentID(data)
	m[id] = data
	return id
}

func TestReadAtVariableChunks(t *testing.T) {
	store := memStore{}
	parts := [][]byte{
		[]byte("01234"),
		[]byte("567"),
		[]byte("89abcde"),
	}
	var blocks []core.BlockID
	var sizes []int64
	var full []byte
	for _, p := range parts {
		blocks = append(blocks, store.put(p))
		sizes = append(sizes, int64(len(p)))
		full = append(full, p...)
	}

	tree := buildTree(store, 4096, []fvsrepo.FileEntry{{
		Path:       "f",
		Mode:       0o644,
		Size:       int64(len(full)),
		Blocks:     blocks,
		BlockSizes: sizes,
	}})

	ino := tree.lookup(1, "f")
	if ino == 0 {
		t.Fatal("file not found in tree")
	}
	n := tree.get(ino)
	if n.offsets == nil {
		t.Fatal("variable-size file did not get offsets")
	}

	cases := []struct {
		off    int64
		length int
		want   string
	}{
		{0, len(full), string(full)},     // whole file
		{0, 3, "012"},                    // inside first block
		{4, 3, "456"},                    // spans first/second boundary
		{5, 3, "567"},                    // exactly the second block
		{7, 4, "789a"},                   // spans second/third boundary
		{int64(len(full)) - 2, 10, "de"}, // clipped at EOF
		{3, 9, "3456789ab"},              // spans all three blocks
	}
	for _, c := range cases {
		got, err := tree.readAt(n, c.off, c.length)
		if err != nil {
			t.Fatalf("readAt(%d,%d): %v", c.off, c.length, err)
		}
		if string(got) != c.want {
			t.Fatalf("readAt(%d,%d) = %q, want %q", c.off, c.length, got, c.want)
		}
	}

	// Out-of-range reads stay empty, not errors.
	if got, err := tree.readAt(n, int64(len(full)), 5); err != nil || len(got) != 0 {
		t.Fatalf("read past EOF: %q %v", got, err)
	}
}

func TestReadAtFixedStillWorks(t *testing.T) {
	store := memStore{}
	bs := 4
	data := []byte("0123456789ab!")
	var blocks []core.BlockID
	for i := 0; i < len(data); i += bs {
		end := i + bs
		if end > len(data) {
			end = len(data)
		}
		blocks = append(blocks, store.put(data[i:end]))
	}

	tree := buildTree(store, bs, []fvsrepo.FileEntry{{
		Path:   "f",
		Mode:   0o644,
		Size:   int64(len(data)),
		Blocks: blocks,
		// No BlockSizes: legacy fixed-size layout.
	}})

	n := tree.get(tree.lookup(1, "f"))
	if n.offsets != nil {
		t.Fatal("fixed-size file must not get offsets")
	}
	got, err := tree.readAt(n, 2, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[2:11]) {
		t.Fatalf("fixed readAt = %q, want %q", got, data[2:11])
	}
}
