//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/single"
)

var procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// attachParentProcess is ATTACH_PARENT_PROCESS, (DWORD)-1.
const attachParentProcess = ^uint32(0)

// idYes is the MessageBox result of the Yes button.
const idYes = 6

// attachConsole connects the standard streams of this GUI-subsystem
// executable to the console of the shell that started it, so command output
// shows up there. Streams the parent already redirected (a pipe or a file)
// are kept.
func attachConsole() {
	outOK, errOK := validStd(windows.STD_OUTPUT_HANDLE), validStd(windows.STD_ERROR_HANDLE)
	if outOK && errOK {
		return
	}
	if r, _, _ := procAttachConsole.Call(uintptr(attachParentProcess)); r == 0 {
		return // not started from a console
	}
	if !outOK {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = f
		}
	}
	if !errOK {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stderr = f
		}
	}
	// The shell printed its prompt already; start on a fresh line.
	fmt.Fprintln(os.Stdout)
}

func validStd(which uint32) bool {
	h, err := windows.GetStdHandle(which)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return false
	}
	t, err := windows.GetFileType(h)
	return err == nil && t != windows.FILE_TYPE_UNKNOWN
}

func msgBox(text string, flags uint32) int32 {
	t, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return 0
	}
	c, err := windows.UTF16PtrFromString(brand.DisplayName)
	if err != nil {
		return 0
	}
	ret, _ := windows.MessageBox(0, t, c, flags|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return ret
}

// confirm asks a yes/no question in a message box. Command-line runs use it
// too: the shell does not wait for this GUI-subsystem program, so reading an
// answer from the console would race with the shell's own prompt.
func confirm(question string) bool {
	return msgBox(question, windows.MB_YESNO|windows.MB_ICONQUESTION) == idYes
}

// messageBox returns the About box used by the tray.
func messageBox() func(title, text string) {
	return func(title, text string) {
		msgBox(title+"\n\n"+text, windows.MB_OK|windows.MB_ICONINFORMATION)
	}
}

// userName is the Windows account name without its domain.
func userName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		if i := strings.LastIndexByte(u.Username, '\\'); i >= 0 {
			return u.Username[i+1:]
		}
		return u.Username
	}
	if n := os.Getenv("USERNAME"); n != "" {
		return n
	}
	return "this user"
}

// offerInstall runs when the tray is started without arguments. A copy that
// is not the installed one asks "Install SyncThing V2 for <user>?"; on yes
// it installs itself (which starts the installed copy with --setup) and this
// process exits. offered is false when the tray should run normally.
func offerInstall(lock *single.Lock) (offered bool, code int) {
	r, err := install.DefaultRoots()
	if err != nil {
		return false, exitOK
	}
	exe, err := os.Executable()
	if err != nil || install.IsInstalledCopy(r, exe) {
		return false, exitOK
	}
	if !confirm(installQuestion()) {
		return false, exitOK
	}
	// The installed copy is started with --setup and needs the lock.
	_ = lock.Release()
	res, err := runInstall(context.Background(), false, false, logWriter{})
	if err != nil {
		msgBox("Installation failed: "+err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		return true, exitFail
	}
	if len(res.Notes) > 0 {
		msgBox(strings.Join(res.Notes, "\n"), windows.MB_OK|windows.MB_ICONINFORMATION)
	}
	return true, exitOK
}

// logWriter sends installer progress lines to the application log.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	applog.Printf("install: %s", strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}
