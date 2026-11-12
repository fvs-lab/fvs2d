package main

import (
	"fmt"
	"github.com/fvs-lab/fvs2d/internal/engine"
	"path/filepath"
	"syscall"
)

type mountServer interface {
	Wait()
	Unmount() error
}
type mountOptions struct {
	Point               string
	Source              string
	Tree                *Tree
	Layers              []Layer
	Upper               string
	Debug               bool
	ClearPrivilegedBits bool
}

func (o mountOptions) validatePaths() error {
	canonical := func(p string) (string, error) {
		p, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(p)
	}
	if err := engine.ValidateUpperStorage(o.Layers, o.Upper); err != nil {
		return err
	}
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && filepath.IsLocal(rel)
	}
	point, err := canonical(o.Point)
	if err != nil {
		return err
	}
	upper := ""
	if o.Upper != "" {
		upper, err = canonical(o.Upper)
		if err != nil {
			return err
		}
		if inside(point, upper) || inside(upper, point) {
			return fmt.Errorf("mount point and upper directory must not overlap: %w", syscall.EINVAL)
		}
	}
	for _, layer := range o.Layers {
		for _, protected := range []string{filepath.Join(layer.Repository, ".fvs2"), layer.Blocks} {
			protected, err = canonical(protected)
			if err != nil {
				return err
			}
			if inside(point, protected) {
				return fmt.Errorf("mount point would hide repository storage: %w", syscall.EINVAL)
			}
		}
	}
	return nil
}
