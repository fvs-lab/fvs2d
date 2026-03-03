package fusefs

import "errors"

var ErrFuse3NotBuilt = errors.New("fuse3 backend not built (compile with: -tags fuse3)")

type Server struct{}

type Config struct {
	MountPoint string
	Debug      bool
	BlocksDir  string
	BlockSize  int
}

func New(_ Config) (*Server, error) { return nil, ErrFuse3NotBuilt }

func (s *Server) MountAndServe() error { return ErrFuse3NotBuilt }

func (s *Server) Close() error { return nil }
