//go:build linux

package osutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

func appDataDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) { // the XDG spec says relative values are invalid
		home, err := HomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, brand.AppDirName()), nil
}

func logDir() (string, error) {
	d, err := appDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "logs"), nil
}

func setPerm(f *os.File, perm os.FileMode) error { return f.Chmod(perm) }

func hiddenAttr() *syscall.SysProcAttr { return nil }

func detachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// xdg-open may stay alive until the launched application exits, so it runs
// detached in its own session and is reaped in the background.
func openURL(u string) error { return xdgOpen(u) }

func openFolder(path string) error { return xdgOpen(path) }

func xdgOpen(target string) error {
	bin, err := exec.LookPath("xdg-open")
	if err != nil {
		return fmt.Errorf("xdg-open is not installed: %w", err)
	}
	_, err = StartDetached(bin, target)
	return err
}
