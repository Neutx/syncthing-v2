//go:build windows

package picker

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	user32  = windows.NewLazySystemDLL("user32.dll")
	ole32   = windows.NewLazySystemDLL("ole32.dll")

	procSHBrowseForFolderW    = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListEx = shell32.NewProc("SHGetPathFromIDListEx")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procCoTaskMemFree         = ole32.NewProc("CoTaskMemFree")
)

const (
	bifReturnOnlyFSDirs = 0x0001
	bifNewDialogStyle   = 0x0040

	bffmInitialized   = 1
	bffmSetSelectionW = 0x0400 + 103 // WM_USER + 103

	hwndTopmost   = ^uintptr(0)
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	maxLongPath   = 32768
	gpfidlDefault = 0
)

type browseInfo struct {
	Owner       uintptr
	Root        uintptr
	DisplayName *uint16
	Title       *uint16
	Flags       uint32
	Callback    uintptr
	LParam      uintptr
	Image       int32
}

// browseCallback preselects the initial folder (passed as lpData) and brings
// the dialog above the topmost dashboard popup.
var browseCallback = windows.NewCallback(func(hwnd, msg, lParam, lpData uintptr) uintptr {
	if msg == bffmInitialized {
		if lpData != 0 {
			procSendMessageW.Call(hwnd, bffmSetSelectionW, 1, lpData)
		}
		procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize)
		procSetForegroundWindow.Call(hwnd)
	}
	return 0
})

type pickResult struct {
	path string
	err  error
}

// folder runs SHBrowseForFolderW on a dedicated OS thread with COM
// initialised as single-threaded apartment, as the new dialog style requires.
func folder(title, initial string) (string, error) {
	done := make(chan pickResult, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := browse(title, initial)
		done <- pickResult{p, err}
	}()
	r := <-done
	return r.path, r.err
}

func browse(title, initial string) (string, error) {
	cerr := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE)
	// S_OK (nil) and S_FALSE (already initialised) both need a matching uninit.
	if cerr == nil || errors.Is(cerr, syscall.Errno(1)) {
		defer windows.CoUninitialize()
	}

	titleW, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return "", err
	}
	display := make([]uint16, windows.MAX_PATH)
	bi := browseInfo{
		DisplayName: &display[0],
		Title:       titleW,
		Flags:       bifReturnOnlyFSDirs | bifNewDialogStyle,
		Callback:    browseCallback,
	}
	var initialW *uint16
	if initial != "" {
		if initialW, err = windows.UTF16PtrFromString(initial); err != nil {
			return "", err
		}
		bi.LParam = uintptr(unsafe.Pointer(initialW))
	}

	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	runtime.KeepAlive(initialW)
	runtime.KeepAlive(titleW)
	runtime.KeepAlive(display)
	if pidl == 0 {
		return "", nil // cancelled
	}
	defer procCoTaskMemFree.Call(pidl)

	buf := make([]uint16, maxLongPath)
	if ok, _, _ := procSHGetPathFromIDListEx.Call(pidl, uintptr(unsafe.Pointer(&buf[0])), maxLongPath, gpfidlDefault); ok == 0 {
		// A virtual folder (such as "This PC") has no file-system path.
		return "", errors.New("picker: the chosen item is not a file-system folder")
	}
	return windows.UTF16ToString(buf), nil
}
