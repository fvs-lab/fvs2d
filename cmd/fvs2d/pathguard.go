package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errPathEscape marks a client-supplied path that resolves outside the
// daemon's configured allowed root. mapError turns it into
// codes.PermissionDenied.
var errPathEscape = errors.New("path escapes allowed root")

// pathGuard canonicalizes client-supplied paths and, when a root is
// configured (--root/--workspace), rejects anything that resolves outside
// it. An empty root disables the check entirely (arbitrary paths allowed),
// matching the pre-sandbox behavior.
type pathGuard struct {
	root string // canonicalized allowed root; "" means unrestricted
}

func newPathGuard(root string) (*pathGuard, error) {
	if root == "" {
		return &pathGuard{}, nil
	}
	canon, err := canonicalizePath(root)
	if err != nil {
		return nil, fmt.Errorf("root: %w", err)
	}
	return &pathGuard{root: canon}, nil
}

// canonicalizePath resolves p to an absolute, symlink-free path. Trailing
// components that do not exist yet (a fresh InitRepository destination, a
// restore destination about to be created) are kept as-is once the nearest
// existing ancestor has been resolved, so a not-yet-created path still gets
// checked against its real, symlink-resolved parent.
func canonicalizePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	var suffix []string
	dir := abs
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(append([]string{resolved}, suffix...)...), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding an existing
			// ancestor; nothing left to resolve symlinks against.
			return abs, nil
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent
	}
}

// check canonicalizes p and, if a root is configured, rejects a path that
// resolves outside it. An empty p is passed through unchanged (callers
// validate presence separately with codes.InvalidArgument).
func (g *pathGuard) check(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	canon, err := canonicalizePath(p)
	if err != nil {
		return "", err
	}
	if g.root == "" {
		return canon, nil
	}
	if canon != g.root && !strings.HasPrefix(canon, g.root+string(os.PathSeparator)) {
		// Deliberately do not include p or g.root in the error: this error
		// text reaches the client verbatim (via mapError ->
		// codes.PermissionDenied), and echoing either would leak the
		// daemon's filesystem layout to any client probing the sandbox
		// boundary.
		return "", errPathEscape
	}
	return canon, nil
}
