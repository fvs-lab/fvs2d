package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	core "fvs-v2-core"
)

// On-disk schema (subset). fvs2d cannot import fvs2/internal/meta (internal,
// different module), so it parses the JSON it needs directly.

type commitFile struct {
	Path   string         `json:"path"`
	Mode   uint32         `json:"mode"`
	Size   int64          `json:"size"`
	Blocks []core.BlockID `json:"blocks"`
	Link   string         `json:"link,omitempty"`
}

type commitDoc struct {
	ID        string       `json:"id"`
	BlockSize int          `json:"block_size"`
	Files     []commitFile `json:"files"`
}

type headDoc struct {
	Type string `json:"type"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

type indexDoc struct {
	Commits []struct {
		ID string `json:"id"`
	} `json:"commits"`
}

// fileNode is a single entry in the mounted tree, addressed by inode number.
type fileNode struct {
	ino        uint64
	name       string
	isDir      bool
	link       string // symlink target, empty for non-symlinks
	mode       uint32 // permission bits only
	size       int64
	blocks     []core.BlockID
	children   map[string]uint64 // dir: name -> child ino
	childOrder []uint64          // dir: child inos sorted by name (stable readdir)
}

// fsTree is a read-only, in-memory view of a committed state.
type fsTree struct {
	nodes     map[uint64]*fileNode
	blockSize int
	store     core.BlockStore
}

func (t *fsTree) get(ino uint64) *fileNode { return t.nodes[ino] }

func (t *fsTree) lookup(parent uint64, name string) uint64 {
	p := t.nodes[parent]
	if p == nil || !p.isDir {
		return 0
	}
	if ino, ok := p.children[name]; ok {
		return ino
	}
	return 0
}

// readAt returns up to length bytes of a file starting at off, fetching only the
// blocks that overlap the requested range from the content-addressed store.
func (t *fsTree) readAt(n *fileNode, off int64, length int) ([]byte, error) {
	if n.isDir || off < 0 || off >= n.size || length <= 0 {
		return nil, nil
	}
	end := off + int64(length)
	if end > n.size {
		end = n.size
	}
	bs := int64(t.blockSize)
	if bs <= 0 {
		bs = 4096
	}
	out := make([]byte, 0, end-off)
	for bi := off / bs; ; bi++ {
		if int(bi) >= len(n.blocks) {
			break
		}
		blkStart := bi * bs
		if blkStart >= end {
			break
		}
		data, err := t.store.Get(n.blocks[bi])
		if err != nil {
			return nil, err
		}
		from := int64(0)
		if off > blkStart {
			from = off - blkStart
		}
		to := int64(len(data))
		if blkStart+to > end {
			to = end - blkStart
		}
		if from < 0 || to > int64(len(data)) || from > to {
			return nil, fmt.Errorf("block %d out of range", bi)
		}
		out = append(out, data[from:to]...)
	}
	return out, nil
}

// buildTree turns a commit's flat file list into an inode tree rooted at ino 1.
func buildTree(store core.BlockStore, blockSize int, files []commitFile) *fsTree {
	t := &fsTree{nodes: map[uint64]*fileNode{}, blockSize: blockSize, store: store}
	root := &fileNode{ino: 1, isDir: true, mode: 0o755, children: map[string]uint64{}}
	t.nodes[1] = root
	var next uint64 = 2

	ensureDir := func(parent uint64, name string) uint64 {
		p := t.nodes[parent]
		if ci, ok := p.children[name]; ok {
			if c := t.nodes[ci]; c != nil && c.isDir {
				return ci
			}
		}
		n := &fileNode{ino: next, name: name, isDir: true, mode: 0o755, children: map[string]uint64{}}
		t.nodes[next] = n
		p.children[name] = next
		next++
		return n.ino
	}

	for _, f := range files {
		clean := strings.Trim(filepath.ToSlash(f.Path), "/")
		if clean == "" {
			continue
		}
		parts := strings.Split(clean, "/")
		parent := uint64(1)
		for i := 0; i < len(parts)-1; i++ {
			parent = ensureDir(parent, parts[i])
		}
		leaf := parts[len(parts)-1]
		n := &fileNode{ino: next, name: leaf, mode: f.Mode, size: f.Size, blocks: f.Blocks, link: f.Link}
		if f.Link != "" {
			n.size = int64(len(f.Link))
		}
		t.nodes[next] = n
		t.nodes[parent].children[leaf] = next
		next++
	}

	// Stable, sorted readdir order for every directory.
	for _, n := range t.nodes {
		if !n.isDir {
			continue
		}
		names := make([]string, 0, len(n.children))
		for name := range n.children {
			names = append(names, name)
		}
		sort.Strings(names)
		n.childOrder = make([]uint64, 0, len(names))
		for _, name := range names {
			n.childOrder = append(n.childOrder, n.children[name])
		}
	}
	return t
}

// buildTreeFromRepo resolves a commit (by prefix, branch, or HEAD) inside a repo
// and builds the mountable tree backed by that repo's block store.
func buildTreeFromRepo(repo, statePrefix, branch, blocksOverride string) (*fsTree, error) {
	metaDir := filepath.Join(repo, ".fvs2")
	if fi, err := os.Stat(metaDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("not an fvs2 repo: %s", repo)
	}
	id, err := resolveCommit(repo, statePrefix, branch)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errors.New("no commit to mount (empty branch or no states)")
	}
	doc, err := loadCommitDoc(repo, id)
	if err != nil {
		return nil, err
	}
	blocks := blocksOverride
	if blocks == "" {
		blocks = filepath.Join(metaDir, "blocks")
	}
	store, err := core.NewDiskBlockStore(blocks)
	if err != nil {
		return nil, err
	}
	bs := doc.BlockSize
	if bs <= 0 {
		bs = 4096
	}
	return buildTree(store, bs, doc.Files), nil
}

func resolveCommit(repo, statePrefix, branch string) (string, error) {
	metaDir := filepath.Join(repo, ".fvs2")
	if statePrefix != "" {
		b, err := os.ReadFile(filepath.Join(metaDir, "index.json"))
		if err != nil {
			return "", err
		}
		var idx indexDoc
		if err := json.Unmarshal(b, &idx); err != nil {
			return "", err
		}
		var hits []string
		for _, c := range idx.Commits {
			if strings.HasPrefix(c.ID, statePrefix) {
				hits = append(hits, c.ID)
			}
		}
		switch len(hits) {
		case 0:
			return "", fmt.Errorf("state not found: %s", statePrefix)
		case 1:
			return hits[0], nil
		default:
			return "", fmt.Errorf("ambiguous state prefix: %s", statePrefix)
		}
	}
	if branch != "" {
		return readBranchRef(metaDir, branch)
	}
	// Fall back to HEAD.
	b, err := os.ReadFile(filepath.Join(metaDir, "HEAD.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return readBranchRef(metaDir, "main")
		}
		return "", err
	}
	var h headDoc
	if err := json.Unmarshal(b, &h); err != nil {
		return "", err
	}
	if h.Type == "commit" {
		return strings.TrimSpace(h.ID), nil
	}
	name := h.Name
	if name == "" {
		name = "main"
	}
	return readBranchRef(metaDir, name)
}

func readBranchRef(metaDir, name string) (string, error) {
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid branch name: %s", name)
	}
	b, err := os.ReadFile(filepath.Join(metaDir, "refs", "heads", name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("branch not found: %s", name)
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func loadCommitDoc(repo, id string) (commitDoc, error) {
	b, err := os.ReadFile(filepath.Join(repo, ".fvs2", "commits", id+".json"))
	if err != nil {
		return commitDoc{}, err
	}
	var c commitDoc
	if err := json.Unmarshal(b, &c); err != nil {
		return commitDoc{}, err
	}
	return c, nil
}
