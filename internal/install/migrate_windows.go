//go:build windows

package install

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DetectLegacy looks for the legacy prototype tray (§6.5): its Startup
// shortcut "Syncthing Tray.lnk" or a running SyncthingTray.exe, both only
// when they refer to r.LegacyExe.
func DetectLegacy(r Roots) Legacy { return detectLegacy(r) }

// MigrateLegacy stops the legacy tray and renames its Startup shortcut to
// "Syncthing Tray.lnk.disabled". Call it only after the user agreed.
func MigrateLegacy(r Roots, l Legacy) error { return migrateLegacy(l, r.Terminate) }

// listProcesses is the production Roots.Processes (Toolhelp snapshot). The
// full path is read only for processes named like the legacy tray.
func listProcesses() ([]Process, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []Process
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pr := Process{PID: int(e.ProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
		if strings.EqualFold(pr.Name, LegacyProcess) {
			pr.Path, _ = processPath(pr.PID)
		}
		out = append(out, pr)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return out, fmt.Errorf("process snapshot: %w", err)
	}
	return out, nil
}

// processPath returns the full executable path of pid.
func processPath(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return imagePath(h)
}

func imagePath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// terminateProcess is the production Roots.Terminate. It checks that the
// PID still runs exe (the legacy tray) right before terminating it, so a
// reused PID or another program of the same name is never killed.
func terminateProcess(pid int, exe string) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	path, err := imagePath(h)
	if err != nil {
		return err
	}
	if exe == "" || !samePath(path, exe) {
		return fmt.Errorf("PID %d is %s, not the legacy tray", pid, filepath.Base(path))
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return nil
}
