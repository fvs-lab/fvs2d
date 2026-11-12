package engine

import "golang.org/x/sys/unix"

type Statistics struct {
	BlockSize       int64  `json:"block_size"`
	IOSize          int64  `json:"io_size"`
	TotalBlocks     uint64 `json:"total_blocks"`
	FreeBlocks      uint64 `json:"free_blocks"`
	AvailableBlocks uint64 `json:"available_blocks"`
	TotalFiles      uint64 `json:"total_files"`
	FreeFiles       uint64 `json:"free_files"`
}

func (s *Engine) Statfs() (Statistics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := Statistics{BlockSize: 4096, IOSize: 1 << 20}
	if s.root == nil {
		return result, nil
	}
	file, err := s.root.Open(".")
	if err != nil {
		return result, err
	}
	defer file.Close()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return result, err
	}
	result.BlockSize = int64(stat.Bsize)
	result.TotalBlocks = uint64(stat.Blocks)
	result.FreeBlocks = uint64(stat.Bfree)
	result.AvailableBlocks = uint64(stat.Bavail)
	result.TotalFiles = uint64(stat.Files)
	result.FreeFiles = uint64(stat.Ffree)
	return result, nil
}
