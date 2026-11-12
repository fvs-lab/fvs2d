package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	fvsrepo "github.com/fvs-lab/fvs2/repo"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const ResourceConfigName = "fvs-mount.json"

type ResourceLayer struct {
	Repository string `json:"repository"`
	State      string `json:"state"`
	Blocks     string `json:"blocks,omitempty"`
}
type ResourceConfig struct {
	Version             int             `json:"version"`
	Layers              []ResourceLayer `json:"layers"`
	Upper               string          `json:"upper,omitempty"`
	ClearPrivilegedBits bool            `json:"clear_privileged_bits,omitempty"`
}

// The FSPathURLResource grants access to one directory. Repository metadata,
// external block stores and upper paths must all resolve inside that resource.
func resourcePath(source, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || !filepath.IsLocal(name) {
		return "", syscall.EINVAL
	}
	target := filepath.Join(source, name)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(source, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("resource path escapes the authorized directory: %w", syscall.EACCES)
	}
	return resolved, nil
}
func LoadResource(source string) (*Engine, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(ResourceConfigName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	var config ResourceConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, syscall.EINVAL
	}
	if config.Version != 1 || len(config.Layers) == 0 {
		return nil, syscall.EINVAL
	}
	layers := make([]Layer, 0, len(config.Layers))
	for _, entry := range config.Layers {
		repository, err := resourcePath(source, entry.Repository)
		if err != nil {
			return nil, err
		}
		if _, err := resourcePath(source, filepath.Join(entry.Repository, ".fvs2")); err != nil {
			return nil, err
		}
		if entry.State == "" {
			return nil, syscall.EINVAL
		}
		configuredBlocks, err := fvsrepo.BlockStorePath(repository)
		if err != nil {
			return nil, err
		}
		configuredRelative, err := filepath.Rel(source, configuredBlocks)
		if err != nil {
			return nil, err
		}
		if _, err := resourcePath(source, configuredRelative); err != nil {
			return nil, fmt.Errorf("repository metadata block store: %w", err)
		}
		layer, err := LoadResolvedLayer(repository, entry.State)
		if err != nil {
			return nil, err
		}
		if entry.Blocks != "" {
			layer.Blocks, err = resourcePath(source, entry.Blocks)
			if err != nil {
				return nil, err
			}
		}
		rel, err := filepath.Rel(source, layer.Blocks)
		if err != nil {
			return nil, err
		}
		blocks, err := resourcePath(source, rel)
		if err != nil {
			return nil, fmt.Errorf("block store: %w", err)
		}
		layer.Blocks = blocks
		layers = append(layers, layer)
	}
	upper := ""
	if config.Upper != "" {
		upper, err = resourcePath(source, config.Upper)
		if err != nil {
			return nil, err
		}
	}
	if err := ValidateUpperStorage(layers, upper); err != nil {
		return nil, err
	}
	tree, err := BuildMergedTreeFromRepos(layers, "")
	if err != nil {
		return nil, err
	}
	e, err := New(tree, upper, config.ClearPrivilegedBits)
	if err != nil {
		return nil, err
	}
	if err := e.ValidateDarwinNames(); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}
func WriteResourceConfig(source string, layers []Layer, upper string, clearPrivilegedBits bool) error {
	if len(layers) == 0 {
		return syscall.EINVAL
	}
	if err := ValidateUpperStorage(layers, upper); err != nil {
		return err
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	relative := func(target string) (string, error) {
		target, err := filepath.Abs(target)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(source, target)
		if err != nil {
			return "", err
		}
		_, err = resourcePath(source, rel)
		return rel, err
	}
	config := ResourceConfig{Version: 1, ClearPrivilegedBits: clearPrivilegedBits}
	for _, layer := range layers {
		rel, err := relative(layer.Repository)
		if err != nil {
			return fmt.Errorf("repository must be inside --source: %w", err)
		}
		blockRel, blockErr := relative(layer.Blocks)
		if err := blockErr; err != nil {
			return fmt.Errorf("block store must be inside --source: %w", err)
		}
		config.Layers = append(config.Layers, ResourceLayer{Repository: rel, State: layer.StateID, Blocks: blockRel})
	}
	if upper != "" {
		config.Upper, err = relative(upper)
		if err != nil {
			return fmt.Errorf("upper must be inside --source: %w", err)
		}
	}
	destination := filepath.Join(source, ResourceConfigName)
	if existing, err := os.ReadFile(destination); err == nil {
		var old ResourceConfig
		if json.Unmarshal(existing, &old) != nil || old.Version != 1 {
			return fmt.Errorf("refusing to replace an unrelated %s", ResourceConfigName)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// The resource configuration is persistent mount input, not snapshot content.
	temporary, err := os.CreateTemp(source, ".fvs-mount-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return err
	}
	dir, err := os.Open(source)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func CommonSource(layers []Layer, upper string) (string, error) {
	var paths []string
	for _, l := range layers {
		paths = append(paths, l.Repository, l.Blocks)
	}
	if upper != "" {
		paths = append(paths, upper)
	}
	if len(paths) == 0 {
		return "", syscall.EINVAL
	}
	source, err := filepath.Abs(paths[0])
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		p, err = filepath.Abs(p)
		if err != nil {
			return "", err
		}
		for {
			rel, err := filepath.Rel(source, p)
			if err == nil && filepath.IsLocal(rel) {
				break
			}
			parent := filepath.Dir(source)
			if parent == source {
				return "", syscall.EACCES
			}
			source = parent
		}
	}
	home, _ := os.UserHomeDir()
	if source == string(filepath.Separator) || strings.TrimRight(source, string(filepath.Separator)) == home {
		return "", fmt.Errorf("common directory is too broad; set --source to a directory containing the layers and upper: %w", syscall.EACCES)
	}
	return source, nil
}

func ValidateUpperStorage(layers []Layer, upper string) error {
	if upper == "" {
		return nil
	}
	upper, err := filepath.Abs(upper)
	if err != nil {
		return err
	}
	upper, err = filepath.EvalSymlinks(upper)
	if err != nil {
		return err
	}
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && filepath.IsLocal(rel)
	}
	for _, layer := range layers {
		for _, protected := range []string{filepath.Join(layer.Repository, ".fvs2"), layer.Blocks} {
			protected, err = filepath.Abs(protected)
			if err != nil {
				return err
			}
			protected, err = filepath.EvalSymlinks(protected)
			if err != nil {
				return err
			}
			if inside(upper, protected) || inside(protected, upper) {
				return fmt.Errorf("upper overlaps repository metadata or block storage: %w", syscall.EINVAL)
			}
		}
	}
	return nil
}
