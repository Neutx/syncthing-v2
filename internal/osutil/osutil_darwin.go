//go:build darwin

package osutil

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

func appDataDir() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", brand.AppDirName()), nil
}

func logDir() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", brand.AppDirName()), nil
}

func setPerm(f *os.File, perm os.FileMode) error { return f.Chmod(perm) }

func hiddenAttr() *syscall.SysProcAttr { return nil }

func detachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// open(1) hands the target to Launch Services and returns at once, so it is
// run synchronously to surface its errors.
func openURL(u string) error { return launchServicesOpen(u) }

func openFolder(path string) error { return launchServicesOpen(folderOpenArgs(path)...) }

// folderOpenArgs returns the open(1) arguments that show the directory path
// in Finder. Launch Services treats a bundle directory (X.app, Y.pkg,
// Z.prefPane and so on) as a document or program, so opening one would
// launch or install it instead of showing a folder. Such a directory is
// therefore revealed (selected in its parent's Finder window, open -R),
// which never launches anything. Bundles are recognised by any name
// extension, since packages are declared by extension, or by a Contents
// subdirectory, the bundle layout; plain folders open normally.
func folderOpenArgs(path string) []string {
	if filepath.Ext(path) != "" {
		return []string{"-R", path}
	}
	if fi, err := os.Lstat(filepath.Join(path, "Contents")); err == nil && fi.IsDir() {
		return []string{"-R", path}
	}
	return []string{path}
}

func launchServicesOpen(args ...string) error {
	_, err := Output(context.Background(), 10*time.Second, "/usr/bin/open", args...)
	return err
}
