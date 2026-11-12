//go:build !darwin

package engine

import "golang.org/x/sys/unix"

const utimeOmit = unix.UTIME_OMIT

func (s *Engine) checkUpperSpelling(string) error { return nil }

func (s *Engine) validateUpperPlatform() error { return nil }
