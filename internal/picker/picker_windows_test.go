//go:build windows

package picker

import (
	"flag"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

var manual = flag.Bool("manual", false, "run tests that open real desktop windows")

var (
	procPostMessageW = user32.NewProc("PostMessageW")
)

const (
	wmCommand = 0x0111
	idOK      = 1
	idCancel  = 2
)

// TestBrowseDialog drives the real SHBrowseForFolderW dialog: it confirms the
// preselected initial folder with OK, then cancels a second dialog. It opens
// windows on the desktop, so it runs only with -manual.
func TestBrowseDialog(t *testing.T) {
	if !*manual {
		t.Skip("run with -manual")
	}
	initial := t.TempDir()
	for _, tc := range []struct {
		button uintptr
		want   string
	}{{idOK, initial}, {idCancel, ""}} {
		type res struct {
			p   string
			err error
		}
		done := make(chan res, 1)
		title := "stv2 picker test"
		go func() {
			p, err := Folder(title, initial)
			done <- res{p, err}
		}()
		dlg := waitForDialog(t)
		// Let BFFM_INITIALIZED finish preselecting the folder.
		time.Sleep(700 * time.Millisecond)
		procPostMessageW.Call(dlg, wmCommand, tc.button, 0)
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("Folder: %v", r.err)
			}
			if !strings.EqualFold(r.p, tc.want) {
				t.Errorf("button %d: Folder = %q, want %q", tc.button, r.p, tc.want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the dialog did not close")
		}
	}
}

func waitForDialog(t *testing.T) uintptr {
	t.Helper()
	pid := windows.GetCurrentProcessId()
	var found uintptr
	cb := windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		var owner uint32
		if _, err := windows.GetWindowThreadProcessId(h, &owner); err != nil || owner != pid {
			return 1
		}
		buf := make([]uint16, 64)
		if n, err := windows.GetClassName(h, &buf[0], int32(len(buf))); err == nil && windows.UTF16ToString(buf[:n]) == "#32770" {
			found = uintptr(h)
			return 0
		}
		return 1
	})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		_ = windows.EnumWindows(cb, nil) // stops early (with an error) once found
		if found != 0 {
			return found
		}
	}
	t.Fatal("the folder dialog did not appear")
	return 0
}
