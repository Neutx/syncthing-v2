//go:build windows

package install

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// shortcut is the content of a .lnk file.
type shortcut struct {
	Target, Args, WorkDir, Description string
	Icon                               string // file holding the icon; index 0 is used
}

var (
	clsidShellLink  = ole.NewGUID("{00021401-0000-0000-C000-000000000046}")
	iidIShellLinkW  = ole.NewGUID("{000214F9-0000-0000-C000-000000000046}")
	iidIPersistFile = ole.NewGUID("{0000010B-0000-0000-C000-000000000046}")
)

// Vtable slots (after IUnknown's QueryInterface, AddRef, Release).
const (
	slSetDescription      = 7
	slSetWorkingDirectory = 9
	slSetArguments        = 11
	slSetIconLocation     = 17
	slSetPath             = 20

	pfSave = 6 // IPersistFile: GetClassID 3, IsDirty 4, Load 5, Save 6
)

// createShortcut writes a shell link through IShellLinkW and IPersistFile.
// COM runs on a dedicated, locked OS thread in a single-threaded apartment,
// as the Shell requires.
func createShortcut(path string, s shortcut) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- createShortcutSTA(path, s)
	}()
	return <-done
}

func createShortcutSTA(path string, s shortcut) error {
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		var oe *ole.OleError
		if !errors.As(err, &oe) || oe.Code() != 1 { // S_FALSE: already initialised
			return fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	defer ole.CoUninitialize()

	link, err := ole.CreateInstance(clsidShellLink, iidIShellLinkW)
	if err != nil {
		return fmt.Errorf("create ShellLink: %w", err)
	}
	defer link.Release()
	obj := unsafe.Pointer(link)

	setters := []struct {
		slot  int
		value string
		name  string
	}{
		{slSetPath, s.Target, "SetPath"},
		{slSetArguments, s.Args, "SetArguments"},
		{slSetWorkingDirectory, s.WorkDir, "SetWorkingDirectory"},
		{slSetDescription, s.Description, "SetDescription"},
	}
	for _, st := range setters {
		if st.value == "" && st.slot != slSetArguments {
			continue
		}
		if err := comCallString(obj, st.slot, st.value); err != nil {
			return fmt.Errorf("IShellLinkW.%s: %w", st.name, err)
		}
	}
	if s.Icon != "" {
		p, err := windows.UTF16PtrFromString(s.Icon)
		if err != nil {
			return err
		}
		err = comCall(obj, slSetIconLocation, uintptr(unsafe.Pointer(p)), 0)
		runtime.KeepAlive(p)
		if err != nil {
			return fmt.Errorf("IShellLinkW.SetIconLocation: %w", err)
		}
	}

	pf, err := link.QueryInterface(iidIPersistFile)
	if err != nil {
		return fmt.Errorf("query IPersistFile: %w", err)
	}
	defer pf.Release()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = comCall(unsafe.Pointer(pf), pfSave, uintptr(unsafe.Pointer(p)), 1)
	runtime.KeepAlive(p)
	if err != nil {
		return fmt.Errorf("IPersistFile.Save: %w", err)
	}
	return nil
}

func comCallString(obj unsafe.Pointer, slot int, s string) error {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return err
	}
	err = comCall(obj, slot, uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	return err
}

// comCall invokes vtable entry slot of the COM object obj.
func comCall(obj unsafe.Pointer, slot int, args ...uintptr) error {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	if int32(hr) < 0 {
		return ole.NewError(hr)
	}
	return nil
}
