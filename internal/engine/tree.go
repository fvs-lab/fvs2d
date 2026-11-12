package engine

import (
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	core "github.com/fvs-lab/core"
	fvsrepo "github.com/fvs-lab/fvs2/repo"
)

// Node is a single entry in the mounted tree, addressed by inode number.
type Node struct {
	Ino     uint64
	Name    string
	IsDir   bool
	Kind    string
	Link    string // symlink target, empty for non-symlinks
	Mode    uint32 // permission bits only
	MtimeNS int64
	Size    int64
	Blocks  []core.BlockID
	// Offsets[i] is the byte offset where Blocks[i] starts; nil for
	// fixed-size (format 1) files.
	Offsets    []int64
	Children   map[string]uint64 // dir: name -> child ino
	ChildOrder []uint64          // dir: child inos sorted by name (stable readdir)
}

type BlockGetter interface {
	Get(id core.BlockID) ([]byte, error)
}

type multiStore struct{ stores []BlockGetter }

func (m multiStore) Get(id core.BlockID) ([]byte, error) {
	var lastErr error
	for _, s := range m.stores {
		data, err := s.Get(id)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, core.ErrBlockNotFound) {
			return nil, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = core.ErrBlockNotFound
	}
	return nil, lastErr
}

// Tree is a read-only, in-memory view of a committed state.
type Tree struct {
	Nodes     map[uint64]*Node
	BlockSize int
	Store     BlockGetter
}

func (t *Tree) Get(ino uint64) *Node { return t.Nodes[ino] }

func (t *Tree) Lookup(parent uint64, name string) uint64 {
	p := t.Nodes[parent]
	if p == nil || !p.IsDir {
		return 0
	}
	if ino, ok := p.Children[name]; ok {
		return ino
	}
	return 0
}

// ReadAt returns up to length bytes of a file starting at off, fetching only the
// blocks that overlap the requested range from the content-addressed store.
func (t *Tree) ReadAt(n *Node, off int64, length int) ([]byte, error) {
	if off < 0 || length < 0 {
		return nil, syscall.EINVAL
	}
	if n == nil {
		return nil, syscall.ENOENT
	}
	if n.IsDir {
		return nil, syscall.EISDIR
	}
	if off >= n.Size || length == 0 {
		return nil, nil
	}
	end := n.Size
	if int64(length) < n.Size-off {
		end = off + int64(length)
	}
	if n.Offsets != nil {
		return t.readAtVariable(n, off, end)
	}
	bs := int64(t.BlockSize)
	if bs <= 0 {
		bs = 4096
	}
	out := make([]byte, 0, end-off)
	for bi := off / bs; ; bi++ {
		if int(bi) >= len(n.Blocks) {
			break
		}
		blkStart := bi * bs
		if blkStart >= end {
			break
		}
		data, err := t.Store.Get(n.Blocks[bi])
		if err != nil {
			return nil, err
		}
		if core.ContentID(data) != n.Blocks[bi] {
			return nil, core.ErrBlockCorrupt
		}
		expected := bs
		if expected > n.Size-blkStart {
			expected = n.Size - blkStart
		}
		if int64(len(data)) != expected {
			return nil, io.ErrUnexpectedEOF
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
	if int64(len(out)) != end-off {
		return nil, io.ErrUnexpectedEOF
	}
	return out, nil
}

// readAtVariable serves [off, end) for a file with variable-size blocks,
// locating the first overlapping block by binary search over the offsets.
func (t *Tree) readAtVariable(n *Node, off, end int64) ([]byte, error) {
	bi := sort.Search(len(n.Offsets), func(i int) bool { return n.Offsets[i] > off }) - 1
	if bi < 0 {
		bi = 0
	}
	out := make([]byte, 0, end-off)
	for ; bi < len(n.Blocks); bi++ {
		blkStart := n.Offsets[bi]
		if blkStart >= end {
			break
		}
		data, err := t.Store.Get(n.Blocks[bi])
		if err != nil {
			return nil, err
		}
		if core.ContentID(data) != n.Blocks[bi] {
			return nil, core.ErrBlockCorrupt
		}
		expected := n.Size - blkStart
		if bi+1 < len(n.Offsets) {
			expected = n.Offsets[bi+1] - blkStart
		}
		if expected <= 0 || int64(len(data)) != expected {
			return nil, io.ErrUnexpectedEOF
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
	if int64(len(out)) != end-off {
		return nil, io.ErrUnexpectedEOF
	}
	return out, nil
}

// BuildTree turns a commit's flat file list into an inode tree rooted at ino 1.
func BuildTree(store BlockGetter, blockSize int, files []fvsrepo.FileEntry) *Tree {
	t := &Tree{Nodes: map[uint64]*Node{}, BlockSize: blockSize, Store: store}
	root := &Node{Ino: 1, IsDir: true, Mode: 0o755, Children: map[string]uint64{}}
	t.Nodes[1] = root
	var next uint64 = 2
	blocks := make(map[core.BlockID]core.BlockID)

	ensureDir := func(parent uint64, name string) uint64 {
		p := t.Nodes[parent]
		if ci, ok := p.Children[name]; ok {
			if c := t.Nodes[ci]; c != nil && c.IsDir {
				return ci
			}
		}
		n := &Node{Ino: next, Name: name, IsDir: true, Mode: 0o755, Children: map[string]uint64{}}
		t.Nodes[next] = n
		p.Children[name] = next
		next++
		return n.Ino
	}

	for _, f := range files {
		clean := strings.Trim(filepath.ToSlash(f.Path), "/")
		if clean == "" {
			continue
		}
		parts := strings.Split(clean, "/")
		parent := uint64(1)
		limit := len(parts) - 1
		if f.Kind == "dir" {
			limit = len(parts)
		}
		for i := 0; i < limit; i++ {
			parent = ensureDir(parent, parts[i])
		}
		if f.Kind == "dir" {
			n := t.Nodes[parent]
			n.MtimeNS = f.ModTimeNS
			if n.MtimeNS == 0 {
				n.MtimeNS = f.ModTime * 1e9
			}
			n.Mode = f.Mode

			continue
		}
		leaf := parts[len(parts)-1]
		for i, block := range f.Blocks {
			if existing, ok := blocks[block]; ok {
				f.Blocks[i] = existing
				continue
			}
			blocks[block] = block
		}
		n := &Node{Ino: next, Name: leaf, Kind: f.Kind, Mode: f.Mode, MtimeNS: f.ModTimeNS, Size: f.Size, Blocks: f.Blocks, Link: f.Link}
		if n.MtimeNS == 0 {
			n.MtimeNS = f.ModTime * 1e9
		}
		if len(f.BlockSizes) == len(f.Blocks) && len(f.Blocks) > 0 {
			offsets := make([]int64, len(f.BlockSizes))
			var pos int64
			for i, s := range f.BlockSizes {
				offsets[i] = pos
				pos += s
			}
			n.Offsets = offsets
		}
		if f.Link != "" {
			n.Size = int64(len(f.Link))
		}
		t.Nodes[next] = n
		t.Nodes[parent].Children[leaf] = next
		next++
	}

	// Stable, sorted readdir order for every directory.
	for _, n := range t.Nodes {
		if !n.IsDir {
			continue
		}
		names := make([]string, 0, len(n.Children))
		for name := range n.Children {
			names = append(names, name)
		}
		sort.Strings(names)
		n.ChildOrder = make([]uint64, 0, len(names))
		for _, name := range names {
			n.ChildOrder = append(n.ChildOrder, n.Children[name])
		}
	}
	return t
}

type LayerSelector struct {
	Repository string
	State      string
	Branch     string
}

func ParseLayerSelector(s string) LayerSelector {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return LayerSelector{Repository: s[:i], State: s[i+1:]}
	}
	if i := strings.LastIndex(s, "#"); i >= 0 {
		return LayerSelector{Repository: s[:i], Branch: s[i+1:]}
	}
	return LayerSelector{Repository: s}
}

const WhiteoutPrefix = ".wh."

// Layer is a layer's commit already resolved and loaded through the
// fvs2 repo package: the mount daemon no longer parses .fvs2 metadata files
// itself (see fvsrepo.ResolveCommit / fvsrepo.DescribeState / fvsrepo.StateFiles).
type Layer struct {
	Repository string
	StateID    string
	Blocks     string
	BlockSize  int
	Files      []fvsrepo.FileEntry
}

// ResolveLayer resolves and loads one layer's commit through the fvs2 repo
// package, so callers that need the same layer for both a manifest response
// and tree construction can resolve it exactly once (see mountManager.create).
func ResolveLayer(sel LayerSelector) (Layer, error) {
	id, err := ResolveLayerState(sel)
	if err != nil {
		return Layer{}, err
	}
	return LoadResolvedLayer(sel.Repository, id)
}

func ResolveLayerState(sel LayerSelector) (string, error) {
	id, err := fvsrepo.ResolveCommit(sel.Repository, sel.State, sel.Branch)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("no commit to mount (empty branch or no states)")
	}
	return id, nil
}

func LoadResolvedLayer(repository, id string) (Layer, error) {
	detail, err := fvsrepo.DescribeState(repository, id)
	if err != nil {
		return Layer{}, err
	}
	files, err := fvsrepo.StateFiles(repository, id)
	if err != nil {
		return Layer{}, err
	}
	blocks, err := fvsrepo.BlockStorePath(repository)
	if err != nil {
		return Layer{}, err
	}
	return Layer{Repository: repository, StateID: id, Blocks: blocks, BlockSize: detail.BlockSize, Files: files}, nil
}

// BuildTreeFromRepo resolves a commit (by prefix, branch, or HEAD) inside a repo
// and builds the mountable tree backed by that repo's block store.
func BuildTreeFromRepo(repository, statePrefix, branch, blocksOverride string) (*Tree, error) {
	layer, err := ResolveLayer(LayerSelector{Repository: repository, State: statePrefix, Branch: branch})
	if err != nil {
		return nil, err
	}
	return BuildMergedTreeFromRepos([]Layer{layer}, blocksOverride)
}

// BuildMergedTreeFromRepos builds a stacked read-only tree from already
// resolved layers (lowest to highest), applying whiteouts. Callers that also
// need each layer's resolved commit id/block size for a response (e.g.
// mountManager.create) should resolve layers once with ResolveLayer and pass
// the results here instead of letting this function re-resolve them.
func BuildMergedTreeFromRepos(layers []Layer, blocksOverride string) (*Tree, error) {
	if len(layers) == 0 {
		return nil, errors.New("no layers to mount")
	}

	files := make(map[string]fvsrepo.FileEntry)
	descendants := make(map[string]int)
	var stores []BlockGetter
	blockSize := 0
	add := func(name string, entry fvsrepo.FileEntry) {
		if _, ok := files[name]; !ok {
			for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
				descendants[parent]++
			}
		}
		files[name] = entry
	}
	remove := func(name string) {
		if _, ok := files[name]; !ok {
			return
		}
		delete(files, name)
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			descendants[parent]--
		}
	}
	del := func(name string, children bool) {
		remove(name)
		if children && descendants[name] > 0 {
			prefix := name + "/"
			for existing := range files {
				if strings.HasPrefix(existing, prefix) {
					remove(existing)
				}
			}
		}
	}

	for _, rc := range layers {
		if err := validateLayer(rc); err != nil {
			return nil, err
		}
		if rc.BlockSize > 0 {
			blockSize = rc.BlockSize
		}
		blocks := blocksOverride
		if blocks == "" {
			blocks = rc.Blocks
		}
		store, err := core.NewDiskBlockStore(blocks)
		if err != nil {
			return nil, err
		}
		stores = append(stores, store)

		for _, f := range rc.Files {
			clean := strings.Trim(filepath.ToSlash(f.Path), "/")
			if clean == "" {
				continue
			}
			if base := path.Base(clean); strings.HasPrefix(base, WhiteoutPrefix) {
				dir := path.Dir(clean)
				if dir == "." {
					dir = ""
				}
				if base == ".wh..wh..opq" {
					prefix := dir
					if prefix != "" {
						prefix += "/"
					}
					for existing := range files {
						if strings.HasPrefix(existing, prefix) && existing != dir {
							remove(existing)
						}
					}
					continue
				}

				target := strings.Trim(path.Join(dir, strings.TrimPrefix(base, WhiteoutPrefix)), "/")
				del(target, true)
			}
		}

		for _, f := range rc.Files {
			clean := strings.Trim(filepath.ToSlash(f.Path), "/")
			if clean == "" {
				continue
			}
			if strings.HasPrefix(path.Base(clean), WhiteoutPrefix) {
				continue
			}
			if f.Kind != "dir" {
				del(clean, true)
			}
			for parent := path.Dir(clean); parent != "."; parent = path.Dir(parent) {
				if entry, ok := files[parent]; ok && entry.Kind != "dir" {
					del(parent, false)
				}
			}
			if len(f.Blocks) > 0 && len(f.BlockSizes) == 0 {
				f.BlockSizes = make([]int64, len(f.Blocks))
				for i := range f.Blocks {
					size := int64(rc.BlockSize)
					if size <= 0 {
						size = 4096
					}
					remaining := f.Size - int64(i)*size
					if remaining < size {
						size = remaining
					}
					f.BlockSizes[i] = size
				}
			}
			add(clean, f)
		}
	}

	if blockSize <= 0 {
		blockSize = 4096
	}
	out := make([]fvsrepo.FileEntry, 0, len(files))
	for _, entry := range files {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return BuildTree(multiStore{stores: stores}, blockSize, out), nil
}
