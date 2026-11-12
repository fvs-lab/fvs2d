package engine

import (
	"os"
	"strings"
)

func (s *Engine) makeOpaque(p string) (result error) {
	i, err := s.rawStat(p, true)
	if err != nil {
		return err
	}
	mode := permissions(s.Mode(i))
	if err := s.root.Chmod(rooted(p), mode|0o700); err != nil {
		return err
	}
	defer func() {
		if err := s.root.Chmod(rooted(p), mode); result == nil {
			result = err
		}
	}()
	dir, err := s.root.Open(rooted(p))
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := s.root.OpenFile(joinPath(p, ".wh..wh..opq"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return s.checkUpperSpelling(joinPath(p, ".wh..wh..opq"))
	}
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return dir.Sync()
}
func (s *Engine) clearDirectoryMarkers(p string) (result error) {
	i, err := s.rawStat(p, true)
	if err != nil {
		return err
	}
	mode := permissions(s.Mode(i))
	if err := s.root.Chmod(p, mode|0o700); err != nil {
		return err
	}
	defer func() {
		if err := s.root.Chmod(p, mode); result == nil {
			result = err
		}
	}()
	dir, err := s.root.Open(p)
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), WhiteoutPrefix) {
			if err := s.root.Remove(joinPath(p, entry.Name())); err != nil {
				return err
			}
		}
	}
	return dir.Sync()
}
