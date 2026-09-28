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
// shortcut "Syncthing Tray.lnk" or a running SyncthingTray.exe.
func DetectLegacy(r Roots) Legacy { return detectLegacy(r.StartupDir, r.Processes) }

// MigrateLegacy stops the legacy tray and renames its Startup shortcut to
// "Syncthing Tray.lnk.disabled". Call it only after the user agreed.
func MigrateLegacy(r Roots, l Legacy) error { return migrateLegacy(l, r.Terminate) }

// listProcesses is the production Roots.Processes (Toolhelp snapshot).
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
		out = append(out, Process{PID: int(e.ProcessID), Name: windows.UTF16ToString(e.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return out, fmt.Errorf("process snapshot: %w", err)
	}
	return out, nil
}

// terminateProcess is the production Roots.Terminate. It checks that the
// PID still belongs to SyncthingTray.exe right before terminating it, so a
// reused PID is never killed.
func terminateProcess(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return err
	}
	if name := filepath.Base(windows.UTF16ToString(buf[:n])); !strings.EqualFold(name, LegacyProcess) {
		return fmt.Errorf("PID %d is %s, not %s", pid, name, LegacyProcess)
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return nil
}
