package runtime

import (
	"os"
	"os/exec"
	"syscall"
)

func DetectFlatpak() bool {
	if os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	if _, err := os.Stat("/.flatpak-info"); err == nil {
		return true
	}
	return false
}

func DevFuseAccessible() bool {
	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}

func FusermountAvailable() bool {
	for _, cmd := range []string{"fusermount3", "fusermount"} {
		if _, err := exec.LookPath(cmd); err == nil {
			return true
		}
	}
	return false
}
