package tailnet

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ErrNotInstalled is returned when the tailscale CLI cannot be found.
var ErrNotInstalled = errors.New("the tailscale command-line tool was not found")

// Locate finds the tailscale CLI: first on PATH, then at the known install
// locations for this OS (on macOS including the CLI inside the app bundle).
func Locate() (string, error) {
	return locate(exec.LookPath, knownPaths(runtime.GOOS, os.Getenv))
}

func locate(lookPath func(string) (string, error), known []string) (string, error) {
	if p, err := lookPath("tailscale"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
	}
	for _, p := range known {
		if isExecutableFile(p) {
			return p, nil
		}
	}
	return "", ErrNotInstalled
}

// knownPaths lists the install locations for goos, in lookup order.
func knownPaths(goos string, getenv func(string) string) []string {
	switch goos {
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
			if pf := getenv(env); filepath.IsAbs(pf) {
				p := filepath.Join(pf, "Tailscale", "tailscale.exe")
				if len(out) == 0 || out[len(out)-1] != p {
					out = append(out, p)
				}
			}
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/opt/homebrew/bin/tailscale",
			"/usr/local/bin/tailscale",
		}
	default:
		return []string{
			"/usr/bin/tailscale",
			"/usr/local/bin/tailscale",
			"/snap/bin/tailscale",
		}
	}
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode().Perm()&0o111 != 0
}
