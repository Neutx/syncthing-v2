//go:build windows

package stinstall

import (
	"context"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// runningSyncthing returns the executable paths of running syncthing.exe
// processes, found with a Toolhelp snapshot and QueryFullProcessImageNameW.
// Processes that cannot be opened (another user's, or protected) are skipped.
func runningSyncthing(ctx context.Context) []string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	var out []string
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if ctx.Err() != nil {
			return out
		}
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "syncthing.exe") {
			continue
		}
		if p := imagePath(pe.ProcessID); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trustedBin reports whether p is an existing regular file. Windows keeps the
// plain existence test: runningSyncthing only reports processes this user may
// open, which excludes other users' processes.
func trustedBin(p string) bool { return isRegular(p) }

func imagePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
