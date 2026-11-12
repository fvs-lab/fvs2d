//go:build darwin

package engine

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Darwin sys/stat.h defines UTIME_OMIT as -2; x/sys does not export it.
const utimeOmit = -2

func (s *Engine) checkUpperSpelling(p string) error {
	if p == "" {
		return nil
	}
	parent, base := splitPath(p)
	dir, err := s.root.Open(rooted(parent))
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == base {
			return nil
		}
	}
	return fmt.Errorf("upper filesystem aliases the requested case or Unicode spelling: %w", syscall.EILSEQ)
}

func (s *Engine) validateUpperPlatform() error {
	if s.root == nil {
		return nil
	}
	name := fmt.Sprintf(".wh..wh..fvs-spelling-%d-\u00e9", time.Now().UnixNano())
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	defer s.root.Remove(name)
	directory, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, actual := range names {
		if actual == name {
			return nil
		}
	}
	return fmt.Errorf("upper filesystem does not preserve Unicode filename spelling: %w", syscall.EILSEQ)
}
