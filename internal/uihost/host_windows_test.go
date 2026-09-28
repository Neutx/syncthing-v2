//go:build windows

package uihost

import (
	"errors"
	"os"
	"sync"
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
