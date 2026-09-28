//go:build windows

package osutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

func localAppData() (string, error) {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0); err == nil && filepath.IsAbs(p) {
		return p, nil
	}
	if p := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(p) {
		return p, nil
	}
	return "", errors.New("cannot determine %LOCALAPPDATA%")
}

func appDataDir() (string, error) {
	base, err := localAppData()
	if err != nil {
		return "", err
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

// setPerm is a no-op on Windows: the per-user profile ACL protects the file,
// and the POSIX mode maps only to the read-only attribute.
func setPerm(*os.File, os.FileMode) error { return nil }

func hiddenAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func openURL(u string) error { return shellOpen(u) }

func openFolder(path string) error { return shellOpen(path) }

// shellOpen runs ShellExecuteW("open") on a dedicated OS thread with COM
// initialised as single-threaded apartment, as the Shell requires.
func shellOpen(target string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cerr := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE)
		// S_OK (nil) and S_FALSE (already initialised) both need a matching uninit.
		if cerr == nil || errors.Is(cerr, syscall.Errno(1)) {
			defer windows.CoUninitialize()
		}
		if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
			done <- fmt.Errorf("ShellExecute %q: %w", target, err)
			return
		}
		done <- nil
	}()
	return <-done
}
