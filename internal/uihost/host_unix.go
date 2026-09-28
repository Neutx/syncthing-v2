//go:build darwin || linux

package uihost

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// On macOS and Linux there is no ui-host child: the dashboard always opens
// in the default browser (open(1) on macOS, xdg-open on Linux) through a
// launch file.
func startPlatform(cfg Config) (Host, error) {
	return newBrowserHost(cfg), nil
}

// RunChild is the `stv2 ui-host` entry point. The child process exists only
// on Windows.
func RunChild() error {
	return errors.New("ui-host is only used on Windows; on macOS and Linux the dashboard opens in the default browser")
}

// Place returns the dashboard size at scale 1. The browser positions its own
// window, so there is no placement to compute.
func Place(sizeDIP image.Point) (image.Rectangle, float64) {
	return image.Rectangle{Max: sizeDIP}, 1
}

// snapDesktopDir is where snapd installs the desktop files of snap apps.
var snapDesktopDir = "/var/lib/snapd/desktop/applications"

// htmlHandler returns the desktop file of the default text/html handler,
// which xdg-open uses for a launch file, or "" if it cannot be told.
var htmlHandler = func() string {
	out, err := osutil.Output(context.Background(), 2*time.Second, "xdg-mime", "query", "default", "text/html")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runtimeDir is $XDG_RUNTIME_DIR/stv2, falling back to the user cache
// directory (~/.cache/stv2 on Linux, ~/Library/Caches/stv2 on macOS). On
// Linux, when the default browser is a confined snap that cannot read those,
// it is the snap's own ~/snap/<name>/common/stv2 instead (see snapLaunchDir).
func runtimeDir() (string, error) {
	if runtime.GOOS == "linux" {
		if home, err := os.UserHomeDir(); err == nil {
			if d := snapLaunchDir(htmlHandler(), home, isSnapDesktop, isPlainDir); d != "" {
				return d, nil
			}
		}
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(d) {
		return filepath.Join(d, brand.BinaryName), nil
	}
	c, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(c) {
		return "", fmt.Errorf("cache directory %q is not absolute", c)
	}
	return filepath.Join(c, brand.BinaryName), nil
}

// isSnapDesktop reports whether snapd installed the desktop file id.
func isSnapDesktop(id string) bool {
	fi, err := os.Stat(filepath.Join(snapDesktopDir, id))
	return err == nil && fi.Mode().IsRegular()
}

// isPlainDir reports whether dir is a directory and not a symbolic link.
func isPlainDir(dir string) bool {
	fi, err := os.Lstat(dir)
	return err == nil && fi.IsDir()
}

// secureDir creates dir with mode 0700 if needed and verifies that it is a
// real directory (not a symlink) owned by the current user, tightening its
// mode to 0700, because launch files carry a login token.
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("launch directory %s is not a plain directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("launch directory %s is not owned by the current user", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}
