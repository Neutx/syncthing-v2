//go:build windows

package uihost

import (
	"errors"
	"os"
	"sync"
	"testing"
)

// When the test binary runs as the real ui-host child for TestManualHost,
// links the page tries to open are recorded in STV2_UIHOST_TEST_LOG instead
// of reaching the desktop's browser. init runs before TestMain starts the
// child.
func init() {
	path := os.Getenv("STV2_UIHOST_TEST_LOG")
	if os.Getenv("STV2_UIHOST_TEST_CHILD") != "real" || path == "" {
		return
	}
	var mu sync.Mutex
	openExternal = func(u string) error {
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(u + "\n")
		return errors.Join(werr, f.Close())
	}
}

// Esc on the loaded dashboard is the page's to handle (back to Status, close
// the folder list, cancel a confirmation); the host must not arm the native
// hide timer, or the popup would vanish after every Esc.
func TestEscOnLoadedPageArmsNoTimer(t *testing.T) {
	old := armEscTimer
	t.Cleanup(func() { armEscTimer = old })
	armed := 0
	armEscTimer = func(uintptr) { armed++ }

	c := &child{visible: true, loaded: true}
	if c.onKey(vkEscape) {
		t.Error("onKey consumed Esc; the page must receive it")
	}
	if c.onKey('A') {
		t.Error("onKey consumed another key")
	}
	if armed != 0 {
		t.Errorf("Esc on the loaded page armed the hide timer %d times", armed)
	}
	if !c.visible {
		t.Error("Esc on the loaded page hid the popup natively")
	}
}
