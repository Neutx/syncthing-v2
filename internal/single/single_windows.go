//go:build windows

package single

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func acquire(name, _ string) (*Lock, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, p)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return nil, ErrAlreadyRunning
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The mutex exists but was created with a security descriptor we
		// cannot open (for example by an elevated copy): still taken.
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, fmt.Errorf("CreateMutex %q: %w", name, err)
	}
	return &Lock{release: func() error { return windows.CloseHandle(h) }}, nil
}
