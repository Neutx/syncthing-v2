//go:build windows

package applog

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// holdOpen opens path the way a log viewer or scanner might: with the given
// share mode, which here never includes FILE_SHARE_DELETE, so the file
// cannot be renamed while the handle is open.
func holdOpen(t *testing.T, path string, share uint32) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRolloverWhileAnotherProcessHoldsTheFile(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fillToEdge(t, l)

	h := holdOpen(t, l.Path(), windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	msg := "written while the file is held\n"
	if _, err := l.Write([]byte(msg)); err != nil {
		t.Fatalf("Write while the file is held: %v", err)
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}

	msg2 := "written after the handle is closed\n"
	if _, err := l.Write([]byte(msg2)); err != nil {
		t.Fatalf("Write after the handle is closed: %v", err)
	}
	prev, err := os.ReadFile(l.Path() + ".1")
	if err != nil {
		t.Fatalf("rollover was not retried: %v", err)
	}
	if !strings.HasSuffix(string(prev), msg) {
		t.Fatal("the line written while the file was held is not in the rolled-over file")
	}
	cur, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != msg2 {
		t.Fatalf("current file = %q, want %q", cur, msg2)
	}
}

func TestRolloverWhenTheFileCannotEvenBeReopened(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fillToEdge(t, l)

	// A scanner that opens the file without FILE_SHARE_WRITE in the moment
	// between rotate's close and reopen leaves the logger without a handle.
	// Reproduce that moment: drop the handle as rotate does, then hold it.
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()
	h := holdOpen(t, l.Path(), windows.FILE_SHARE_READ)
	if _, err := l.Write([]byte("lost\n")); err == nil {
		t.Fatal("Write succeeded although the file could not be reopened")
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}

	msg := "logging resumes once the file is released\n"
	if _, err := l.Write([]byte(msg)); err != nil {
		t.Fatalf("Write after the handle is closed: %v", err)
	}
	l.Printf("and keeps going")
	cur, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(cur), "and keeps going\n") || !strings.Contains(string(cur), msg) {
		t.Fatalf("current file = %q", cur)
	}
}
