//go:build darwin

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	fvsrepo "github.com/fvs-lab/fvs2/repo"
)

func TestUpperRejectsNativeAliases(t *testing.T) {
	for _, names := range [][2]string{{"case", "CASE"}, {"\u00e9", "e\u0301"}} {
		e, _, _ := fixture(t, true)
		if err := e.ValidateDarwinNames(); err != nil {
			t.Fatal(err)
		}
		original := filepath.Join(e.upper, names[0])
		if err := os.WriteFile(original, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, aliasErr := os.Lstat(filepath.Join(e.upper, names[1]))
		_, err := e.Lookup(names[1])
		if aliasErr == nil {
			if !errors.Is(err, syscall.EILSEQ) {
				t.Fatalf("lookup aliased %q: %v", names[0], err)
			}
			if _, err := e.Create(names[1], "file", "", 0o600); !errors.Is(err, syscall.EILSEQ) {
				t.Fatalf("create aliased %q: %v", names[0], err)
			}
		} else if !os.IsNotExist(aliasErr) {
			t.Fatal(aliasErr)
		} else if !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("distinct spelling lookup: %v", err)
		}
		data, err := os.ReadFile(original)
		if err != nil || string(data) != "original" {
			t.Fatalf("alias access changed the upper: %q %v", data, err)
		}
	}
}

func TestUpperWhiteoutMatchesExactSpelling(t *testing.T) {
	for _, names := range [][2]string{{"Case", "case"}, {"\u00e9", "e\u0301"}} {
		store := memStore{}
		tree := BuildTree(store, 4096, []fvsrepo.FileEntry{{Path: names[0], Mode: 0o600}, {Path: names[1], Mode: 0o600}})
		e, err := New(tree, t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.Close() })
		if err := e.ValidateDarwinNames(); err != nil {
			t.Fatal(err)
		}
		if err := e.Remove(names[0], false); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Lookup(names[1]); err != nil {
			t.Fatalf("whiteout of %q hid %q: %v", names[0], names[1], err)
		}
		_, aliasErr := os.Lstat(filepath.Join(e.upper, WhiteoutPrefix+names[1]))
		if err := e.Remove(names[1], false); aliasErr == nil && !errors.Is(err, syscall.EILSEQ) {
			t.Fatalf("unrepresentable second whiteout: %v", err)
		} else if os.IsNotExist(aliasErr) && err != nil {
			t.Fatal(err)
		}
	}
}
