package main

import (
	"errors"
	"fmt"
	fvsrepo "fvs2/repo"
	"path"
	"path/filepath"
	"sort"
	"strings"

	core "fvs-v2-core"
)

// fileNode is a single entry in the mounted tree, addressed by inode number.
type fileNode struct {
	ino    uint64
	name   string
	isDir  bool
	link   string // symlink target, empty for non-symlinks
	mode   uint32 // permission bits only
	size   int64
	blocks []core.BlockID
	// offsets[i] is the byte offset where blocks[i] starts; nil for
	// fixed-size (format 1) files.
	offsets    []int64
	children   map[string]uint64 // dir: name -> child ino
	childOrder []uint64          // dir: child inos sorted by name (stable readdir)
}

type blockGetter interface {
	Get(id core.BlockID) ([]byte, error)
}

type multiStore struct{ stores []blockGetter }

func (m multiStore) Get(id core.BlockID) ([]byte, error) {
	var lastErr error
	for _, s := range m.stores {
		data, err := s.Get(id)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = core.ErrBlockNotFound
	}
	return nil, lastErr
}

// fsTree is a read-only, in-memory view of a committed state.
type fsTree struct {
	nodes     map[uint64]*fileNode
	blockSize int
	store     blockGetter
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
	if n.offsets != nil {
		return t.readAtVariable(n, off, end)
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

// readAtVariable serves [off, end) for a file with variable-size blocks,
// locating the first overlapping block by binary search over the offsets.
func (t *fsTree) readAtVariable(n *fileNode, off, end int64) ([]byte, error) {
	bi := sort.Search(len(n.offsets), func(i int) bool { return n.offsets[i] > off }) - 1
	if bi < 0 {
		bi = 0
	}
	out := make([]byte, 0, end-off)
	for ; bi < len(n.blocks); bi++ {
		blkStart := n.offsets[bi]
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
func buildTree(store blockGetter, blockSize int, files []fvsrepo.FileEntry) *fsTree {
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
		if len(f.BlockSizes) == len(f.Blocks) && len(f.Blocks) > 0 {
			offsets := make([]int64, len(f.BlockSizes))
			var pos int64
			for i, s := range f.BlockSizes {
				offsets[i] = pos
				pos += s
			}
			n.offsets = offsets
		}
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

type layerSel struct {
	repo   string
	state  string
	branch string
}

func parseLayerSel(s string) layerSel {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return layerSel{repo: s[:i], state: s[i+1:]}
	}
	if i := strings.LastIndex(s, "#"); i >= 0 {
		return layerSel{repo: s[:i], branch: s[i+1:]}
	}
	return layerSel{repo: s}
}

const whiteoutPrefix = ".wh."

// resolvedCommit is a layer's commit already resolved and loaded through the
// fvs2 repo package: the mount daemon no longer parses .fvs2 metadata files
// itself (see fvsrepo.ResolveCommit / fvsrepo.DescribeState / fvsrepo.StateFiles).
type resolvedCommit struct {
	repo      string
	stateID   string
	blockSize int
	files     []fvsrepo.FileEntry
}

// resolveLayer resolves and loads one layer's commit through the fvs2 repo
// package, so callers that need the same layer for both a manifest response
// and tree construction can resolve it exactly once (see mountManager.create).
func resolveLayer(sel layerSel) (resolvedCommit, error) {
	id, err := fvsrepo.ResolveCommit(sel.repo, sel.state, sel.branch)
	if err != nil {
		return resolvedCommit{}, err
	}
	if id == "" {
		return resolvedCommit{}, errors.New("no commit to mount (empty branch or no states)")
	}
	detail, err := fvsrepo.DescribeState(sel.repo, id)
	if err != nil {
		return resolvedCommit{}, err
	}
	files, err := fvsrepo.StateFiles(sel.repo, id)
	if err != nil {
		return resolvedCommit{}, err
	}
	return resolvedCommit{repo: sel.repo, stateID: id, blockSize: detail.BlockSize, files: files}, nil
}

// buildTreeFromRepo resolves a commit (by prefix, branch, or HEAD) inside a repo
// and builds the mountable tree backed by that repo's block store.
func buildTreeFromRepo(repo, statePrefix, branch, blocksOverride string) (*fsTree, error) {
	rc, err := resolveLayer(layerSel{repo: repo, state: statePrefix, branch: branch})
	if err != nil {
		return nil, err
	}
	blocks := blocksOverride
	if blocks == "" {
		blocks = filepath.Join(repo, ".fvs2", "blocks")
	}
	store, err := core.NewDiskBlockStore(blocks)
	if err != nil {
		return nil, err
	}
	bs := rc.blockSize
	if bs <= 0 {
		bs = 4096
	}
	return buildTree(store, bs, rc.files), nil
}

// buildMergedTreeFromRepos builds a stacked read-only tree from already
// resolved layers (lowest to highest), applying whiteouts. Callers that also
// need each layer's resolved commit id/block size for a response (e.g.
// mountManager.create) should resolve layers once with resolveLayer and pass
// the results here instead of letting this function re-resolve them.
func buildMergedTreeFromRepos(layers []resolvedCommit, blocksOverride string) (*fsTree, error) {
	if len(layers) == 0 {
		return nil, errors.New("no layers to mount")
	}

	order := make([]string, 0)
	files := make(map[string]fvsrepo.FileEntry)
	var stores []blockGetter
	blockSize := 0

	add := func(p string, f fvsrepo.FileEntry) {
		if _, ok := files[p]; !ok {
			order = append(order, p)
		}
		files[p] = f
	}
	del := func(p string) {
		if _, ok := files[p]; !ok {
			return
		}
		delete(files, p)
		for i, q := range order {
			if q == p {
				order = append(order[:i], order[i+1:]...)
				break
			}
		}
	}

	for _, rc := range layers {
		if rc.blockSize > 0 {
			blockSize = rc.blockSize
		}
		blocks := blocksOverride
		if blocks == "" {
			blocks = filepath.Join(rc.repo, ".fvs2", "blocks")
		}
		store, err := core.NewDiskBlockStore(blocks)
		if err != nil {
			return nil, err
		}
		stores = append(stores, store)

		for _, f := range rc.files {
			clean := strings.Trim(filepath.ToSlash(f.Path), "/")
			if clean == "" {
				continue
			}
			if base := path.Base(clean); strings.HasPrefix(base, whiteoutPrefix) {
				target := strings.Trim(path.Join(path.Dir(clean), strings.TrimPrefix(base, whiteoutPrefix)), "/")
				del(target)
				continue
			}
			add(clean, f)
		}
	}

	if blockSize <= 0 {
		blockSize = 4096
	}
	out := make([]fvsrepo.FileEntry, 0, len(order))
	for _, p := range order {
		out = append(out, files[p])
	}
	return buildTree(multiStore{stores: stores}, blockSize, out), nil
}
