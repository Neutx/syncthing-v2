//go:build darwin

package install

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
)

// DefaultRoots returns the current user's real locations:
// ~/Library/Application Support/SyncThingV2, ~/Library/Logs/SyncThingV2,
// ~/Applications and the LaunchAgents directory.
func DefaultRoots() (Roots, error) {
	data, err := osutil.AppDataDir()
	if err != nil {
		return Roots{}, err
	}
	logs, err := osutil.LogDir()
	if err != nil {
		return Roots{}, err
	}
	home, err := osutil.HomeDir()
	if err != nil {
		return Roots{}, err
	}
	managed, err := stinstall.ManagedDir()
	if err != nil {
		return Roots{}, err
	}
	ar, err := autostart.DefaultRoots()
	if err != nil {
		return Roots{}, err
	}
	return Roots{
		InstallDir:          filepath.Join(data, "bin"),
		DataDir:             data,
		LogDir:              logs,
		ManagedSyncthingDir: managed,
		Autostart:           autostart.New(ar),
		UserApplications:    filepath.Join(home, "Applications"),
	}, nil
}

// bundleOf returns the .app bundle that contains exe
// (<X>.app/Contents/MacOS/stv2), or "".
func bundleOf(exe string) string {
	macos := filepath.Dir(filepath.Clean(exe))
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) == "MacOS" && filepath.Base(contents) == "Contents" && strings.HasSuffix(strings.ToLower(app), ".app") {
		return app
	}
	return ""
}

// placeBinary uses the binary inside its .app bundle where it is (the .app
// was dragged to Applications or moved to ~/Applications by install.sh). A
// bundle still on the disk image or in an App Translocation copy is refused
// (CheckLoginPath), because the login item would point at a path that goes
// away. A bare binary (a development build) is copied to <InstallDir>/stv2.
func placeBinary(r Roots, src string) (string, bool, error) {
	if app := bundleOf(src); app != "" {
		if err := CheckLoginPath(src); err != nil {
			return "", false, fmt.Errorf("install: %s: %w", filepath.Base(app), err)
		}
		return src, false, nil
	}
	dst := InstalledExe(r)
	if samePath(src, dst) {
		return dst, false, nil
	}
	upgraded := isRegular(dst)
	if err := copyExecutable(src, dst); err != nil {
		return "", false, fmt.Errorf("install: copy %s: %w", filepath.Base(src), err)
	}
	return dst, upgraded, nil
}

// register has nothing to write on macOS: the .app is the registration and
// the LaunchAgent is the tray autostart entry.
func register(Roots, string) ([]string, error) { return nil, nil }

func unregister(Roots) error { return nil }

// removeProgram deletes a copied bare binary and, when the running binary
// belongs to a bundle in ~/Applications, that bundle.
func removeProgram(r Roots, o Options, _ bool, _ []string) ([]string, error) {
	var errs []error
	errs = append(errs, removeIfExists(InstalledExe(r)), removeIfEmpty(r.InstallDir))
	if app := bundleOf(o.Self); app != "" && r.UserApplications != "" && samePath(filepath.Dir(app), r.UserApplications) {
		errs = append(errs, removeAllRetry(app, 0))
	} else if app != "" {
		return []string{"Move " + app + " to the Trash to finish."}, errors.Join(errs...)
	}
	return nil, errors.Join(errs...)
}

func selfDelete(string) error { return errors.New("self-delete is only used on Windows") }

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Inspect reports the tray login item; the .app itself can live anywhere.
func Inspect(r Roots) State {
	var s State
	if r.Autostart != nil {
		if on, err := r.Autostart.Enabled(autostart.Tray); err == nil && on {
			s.Installed = true
		}
	}
	if exe := InstalledExe(r); isRegular(exe) {
		s.Installed, s.Exe = true, exe
	}
	return s
}

// WebView2Available is Windows only; the macOS dashboard uses the browser.
func WebView2Available() bool { return false }
