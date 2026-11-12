//go:build darwin

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ExtensionID = "lab.fvs.FVS.FS"

func DetectFlatpak() bool       { return false }
func DevFuseAccessible() bool   { return false }
func FusermountAvailable() bool { return false }
func fsKitHelper() (string, error) {
	var candidates []string
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "fvsfsctl"))
	}
	if app := os.Getenv("FVS_APP_PATH"); app != "" {
		candidates = append(candidates, filepath.Join(app, "Contents/MacOS/fvsfsctl"))
	}
	candidates = append(candidates, "/Applications/FVS.app/Contents/MacOS/fvsfsctl")
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications/FVS.app/Contents/MacOS/fvsfsctl"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("FVS.app is not installed; install the signed app or set FVS_APP_PATH")
}
func Probe() Capabilities {
	c := Capabilities{Backend: "fskit", MinimumMacOS: "26.0"}
	data, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		c.Diagnostic = fmt.Sprintf("cannot determine macOS version: %v", err)
		return c
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(string(data)), ".")[0])
	if err != nil || major < 26 {
		c.Diagnostic = "FSKit repository mounts require macOS 26.0 or later (FSPathURLResource)"
		return c
	}
	helper, err := fsKitHelper()
	if err != nil {
		c.Diagnostic = err.Error()
		return c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err = exec.CommandContext(ctx, helper).Output()
	var state struct {
		Registered bool   `json:"registered"`
		Enabled    bool   `json:"enabled"`
		Error      string `json:"error"`
	}
	decodeErr := json.Unmarshal(data, &state)
	if err != nil || decodeErr != nil {
		c.Diagnostic = fmt.Sprintf("cannot query FSKit extensions: %s (%v)", state.Error, err)
		return c
	}
	if !state.Registered {
		c.Diagnostic = "FVS filesystem extension is not registered; install and open FVS.app"
		return c
	}
	if !state.Enabled {
		c.Diagnostic = "FVS filesystem extension is disabled; enable it in System Settings > General > Login Items & Extensions > File System Extensions"
		return c
	}
	c.Available = true
	c.Diagnostic = "FSKit extension enabled; folder authorization and activation are checked by mount"
	return c
}
