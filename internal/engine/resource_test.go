package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	core "github.com/fvs-lab/core"
	fvsrepo "github.com/fvs-lab/fvs2/repo"
)

func resourceFixture(t *testing.T) (string, Layer) {
	t.Helper()
	source := t.TempDir()
	repository, err := fvsrepo.Init(filepath.Join(source, "repo"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := fvsrepo.BeginSnapshot(repository.Path, fvsrepo.SnapshotOptions{Message: "resource"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(fvsrepo.Entry{Path: "file", Kind: fvsrepo.EntryFile, Mode: 0o640, Size: 4}, bytes.NewReader([]byte("base"))); err != nil {
		t.Fatal(err)
	}
	result, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	layer, err := LoadResolvedLayer(repository.Path, result.StateID)
	if err != nil {
		t.Fatal(err)
	}
	return source, layer
}
func TestResourceConfigurationPinsStatesAndConfinesPaths(t *testing.T) {
	source, layer := resourceFixture(t)
	upper := filepath.Join(source, "upper")
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteResourceConfig(source, []Layer{layer}, upper, true); err != nil {
		t.Fatal(err)
	}
	e, err := LoadResource(source)
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.Open("file", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt([]byte("UP"), 0); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, err = LoadResource(source)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	h, err = e.Open("file", false)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := h.ReadAt(buf, 0); err != nil || string(buf) != "UPse" {
		t.Fatalf("persisted upper %q %v", buf, err)
	}
	outside := t.TempDir()
	if err := WriteResourceConfig(source, []Layer{layer}, outside, false); err == nil {
		t.Fatal("accepted outside upper")
	}
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := resourcePath(source, "escape"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("escape: %v", err)
	}
	config := ResourceConfig{Version: 1, Layers: []ResourceLayer{{Repository: "../outside", State: layer.StateID}}}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(source, ResourceConfigName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadResource(source); err == nil {
		t.Fatal("accepted parent traversal")
	}
}
func TestResourceRejectsInvalidConfiguration(t *testing.T) {
	source, _ := resourceFixture(t)
	for _, data := range []string{`{"version":1,"layers":[]}`, `{"version":1,"layers":[],"unknown":1}`, `{} {}`, `invalid`} {
		if err := os.WriteFile(filepath.Join(source, ResourceConfigName), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadResource(source); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if err := WriteResourceConfig(source, []Layer{{Repository: source, Blocks: source, StateID: "irrelevant"}}, "", false); err == nil {
		t.Fatal("overwrote unrelated config")
	}
}

func TestResourceRejectsUpperInsideLowerStorage(t *testing.T) {
	source, layer := resourceFixture(t)
	config := ResourceConfig{Version: 1, Layers: []ResourceLayer{{Repository: "repo", State: layer.StateID}}, Upper: "repo/.fvs2/blocks"}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(source, ResourceConfigName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadResource(source); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("upper overlaps lower: %v", err)
	}
}
func TestResourceReadDetectsDiskCorruption(t *testing.T) {
	source, layer := resourceFixture(t)
	if err := WriteResourceConfig(source, []Layer{layer}, "", false); err != nil {
		t.Fatal(err)
	}
	e, err := LoadResource(source)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	block := layer.Files[0].Blocks[0]
	if err := os.WriteFile(filepath.Join(layer.Blocks, string(block)), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := e.Open("file", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, core.ErrBlockCorrupt) {
		t.Fatalf("corrupt disk data: %v", err)
	}
}
