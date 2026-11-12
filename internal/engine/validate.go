package engine

import (
	"fmt"
	"math"
	"path"
	"strings"
	"syscall"
)

func validateLayer(layer Layer) error {
	entries := map[string]string{}
	for _, f := range layer.Files {
		if f.Path == "" || strings.HasPrefix(f.Path, "/") || path.Clean(f.Path) != f.Path {
			return fmt.Errorf("invalid snapshot path %q: %w", f.Path, syscall.EINVAL)
		}
		parts := strings.Split(f.Path, "/")
		for i, name := range parts {
			if name == "" || name == "." || name == ".." || strings.ContainsRune(name, 0) {
				return syscall.EINVAL
			}
			if strings.HasPrefix(name, WhiteoutPrefix) && (i != len(parts)-1 || name == WhiteoutPrefix) {
				return syscall.EINVAL
			}
		}
		if _, ok := entries[f.Path]; ok {
			return fmt.Errorf("duplicate snapshot entry %q", f.Path)
		}
		entries[f.Path] = f.Kind
		if strings.ContainsRune(f.Link, 0) {
			return syscall.EINVAL
		}
		if f.Size < 0 {
			return syscall.EINVAL
		}
		switch f.Kind {
		case "", "file", "dir", "symlink", "fifo":
		default:
			return fmt.Errorf("unsupported snapshot kind %q: %w", f.Kind, syscall.ENOTSUP)
		}
		if f.Kind == "dir" || f.Kind == "fifo" || f.Kind == "symlink" || f.Link != "" {
			continue
		}
		if f.Size == 0 && len(f.Blocks) != 0 {
			return syscall.EINVAL
		}
		if f.Size > 0 && len(f.Blocks) == 0 {
			return syscall.EINVAL
		}
		if len(f.BlockSizes) > 0 {
			if len(f.BlockSizes) != len(f.Blocks) {
				return syscall.EINVAL
			}
			var total int64
			for _, size := range f.BlockSizes {
				if size <= 0 || size > math.MaxInt64-total {
					return syscall.EINVAL
				}
				total += size
			}
			if total != f.Size {
				return fmt.Errorf("snapshot entry %q has inconsistent block lengths: %w", f.Path, syscall.EINVAL)
			}
		} else if len(f.Blocks) > 0 {
			bs := int64(layer.BlockSize)
			if bs <= 0 {
				bs = 4096
			}
			if (f.Size-1)/bs+1 != int64(len(f.Blocks)) {
				return syscall.EINVAL
			}
		}
	}
	for name := range entries {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if kind, ok := entries[parent]; ok && kind != "dir" {
				return fmt.Errorf("snapshot entry %q has a non-directory parent", name)
			}
		}
	}
	return nil
}
