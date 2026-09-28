//go:build linux

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/icon"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
)

// IconSizes are the hicolor sizes written on install.
var IconSizes = []int{16, 22, 24, 32, 48, 64, 128, 256, 512}

// DefaultPackagedDirs are prefixes of binaries owned by a package manager.
var DefaultPackagedDirs = []string{"/usr/", "/opt/", "/snap/"}

// DefaultRoots returns the current user's real locations:
// $XDG_DATA_HOME (default ~/.local/share)/syncthing-v2/bin, ~/.local/bin,
// $XDG_DATA_HOME/applications and $XDG_DATA_HOME/icons/hicolor.
func DefaultRoots() (Roots, error) {
	data, err := osutil.AppDataDir()
	if err != nil {
		return Roots{}, err
	}
	home, err := osutil.HomeDir()
	if err != nil {
		return Roots{}, err
	}
	xdgData := filepath.Dir(data)
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
		ManagedSyncthingDir: managed,
		Autostart:           autostart.New(ar),
		LinkDir:             filepath.Join(home, ".local", "bin"),
		ApplicationsDir:     filepath.Join(xdgData, "applications"),
		IconsDir:            filepath.Join(xdgData, "icons", "hicolor"),
		PackagedDirs:        DefaultPackagedDirs,
	}, nil
}

// packaged reports whether exe was installed by a package manager (the
// .deb's /usr/bin/stv2). Such a copy is used in place: the package already
// provides the PATH entry, the menu entry and the icons.
func packaged(r Roots, exe string) bool {
	for _, p := range r.PackagedDirs {
		if strings.HasPrefix(filepath.Clean(exe)+"/", p) {
			return true
		}
	}
	return false
}

// placeBinary copies src to <InstallDir>/stv2 (a rename over the old file,
// which a running tray keeps using until it exits). A package-managed src is
// used where it is.
func placeBinary(r Roots, src string) (string, bool, error) {
	if packaged(r, src) {
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

// register writes the ~/.local/bin/stv2 symlink, the .desktop file and the
// hicolor icons (rendered by the same code that generated assets/icons).
func register(r Roots, exe string) ([]string, error) {
	if packaged(r, exe) {
		return nil, nil
	}
	if strings.ContainsAny(exe, "\r\n") {
		return nil, fmt.Errorf("install: %q contains a line break", exe)
	}
	var notes []string
	if r.LinkDir != "" {
		link := filepath.Join(r.LinkDir, brand.BinaryName)
		fi, err := os.Lstat(link)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink == 0:
			notes = append(notes, link+" exists and is not a symlink; it was left alone.")
		default:
			if err := os.MkdirAll(r.LinkDir, 0o755); err != nil {
				return nil, fmt.Errorf("install: %w", err)
			}
			if err := removeIfExists(link); err != nil {
				return nil, fmt.Errorf("install: %w", err)
			}
			if err := os.Symlink(exe, link); err != nil {
				return nil, fmt.Errorf("install: symlink %s: %w", link, err)
			}
			if !onPath(r.LinkDir) {
				notes = append(notes, r.LinkDir+" is not on your PATH; add it to run "+brand.BinaryName+" from a terminal.")
			}
		}
	}
	if r.IconsDir != "" {
		for _, n := range IconSizes {
			p := iconPath(r, n)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return notes, fmt.Errorf("install: icons: %w", err)
			}
			if err := osutil.WriteFileAtomic(p, icon.PNG(icon.Ring(model.StateInSync, 100, n)), 0o644); err != nil {
				return notes, fmt.Errorf("install: icons: %w", err)
			}
		}
	}
	if r.ApplicationsDir != "" {
		if err := os.MkdirAll(r.ApplicationsDir, 0o755); err != nil {
			return notes, fmt.Errorf("install: %w", err)
		}
		if err := osutil.WriteFileAtomic(desktopPath(r), desktopFile(exe), 0o644); err != nil {
			return notes, fmt.Errorf("install: desktop file: %w", err)
		}
	}
	return notes, nil
}

func iconPath(r Roots, n int) string {
	sz := strconv.Itoa(n)
	return filepath.Join(r.IconsDir, sz+"x"+sz, "apps", brand.BinaryName+".png")
}

func desktopPath(r Roots) string {
	return filepath.Join(r.ApplicationsDir, brand.BinaryName+".desktop")
}

// desktopFile is the menu entry; it matches packaging/linux/stv2.desktop
// except that Exec names the installed binary.
func desktopFile(exe string) []byte {
	return []byte("[Desktop Entry]\n" +
		"Type=Application\n" +
		"Version=1.0\n" +
		"Name=" + brand.DisplayName + "\n" +
		"GenericName=Folder Sync Tray\n" +
		"Comment=Pair Syncthing devices over Tailscale and see sync status in the tray\n" +
		"Exec=" + desktopExecArg(exe) + "\n" +
		"Icon=" + brand.BinaryName + "\n" +
		"Terminal=false\n" +
		"Categories=Network;FileTransfer;Utility;\n" +
		"Keywords=syncthing;tailscale;sync;folder;tray;\n" +
		"StartupNotify=false\n")
}

// desktopExecArg quotes one Exec argument following the Desktop Entry
// specification: reserved characters force double quotes, inside which ",
// `, $ and \ are backslash-escaped; % is doubled.
func desktopExecArg(a string) string {
	a = strings.ReplaceAll(a, "%", "%%")
	if a != "" && !strings.ContainsAny(a, " \t\n\"'\\><~|&;$*?#()`") {
		return a
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`)
	return `"` + r.Replace(a) + `"`
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if samePath(p, dir) {
			return true
		}
	}
	return false
}

// unregister removes the symlink (only when it still points at our
// binary), the .desktop file and the icons.
func unregister(r Roots) error {
	var errs []error
	if r.LinkDir != "" {
		link := filepath.Join(r.LinkDir, brand.BinaryName)
		if target, err := os.Readlink(link); err == nil && within(target, r.InstallDir) {
			errs = append(errs, removeIfExists(link))
		}
	}
	if r.ApplicationsDir != "" {
		errs = append(errs, removeIfExists(desktopPath(r)))
	}
	if r.IconsDir != "" {
		for _, n := range IconSizes {
			errs = append(errs, removeIfExists(iconPath(r, n)))
		}
	}
	return errors.Join(errs...)
}

// removeProgram deletes the installed binary; InstallDir usually lies in
// DataDir and is gone already. A running binary can be unlinked on Linux.
func removeProgram(r Roots, _ Options, _ bool, _ []string) ([]string, error) {
	if err := removeIfExists(InstalledExe(r)); err != nil {
		return nil, err
	}
	return nil, removeIfEmpty(r.InstallDir)
}

func selfDelete(string) error { return errors.New("self-delete is only used on Windows") }

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Inspect reads the installation under r.
func Inspect(r Roots) State {
	var s State
	if exe := InstalledExe(r); isRegular(exe) {
		s.Installed, s.Exe = true, exe
	}
	if r.ApplicationsDir != "" && isRegular(desktopPath(r)) {
		s.Shortcut = desktopPath(r)
	}
	return s
}

// WebView2Available is Windows only; the Linux dashboard uses the browser.
func WebView2Available() bool { return false }
