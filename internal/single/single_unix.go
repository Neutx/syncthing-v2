//go:build darwin || linux

package single

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// lockPath maps a mutex-style name such as `Local\SyncThingV2.Tray` to
// dir/SyncThingV2.Tray.lock.
func lockPath(name, dir string) string {
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, name)
	return filepath.Join(dir, clean+".lock")
}

func acquire(name, dir string) (*Lock, error) {
	if dir == "" {
		return nil, errors.New("single: empty lock directory")
	}
	if err := osutil.EnsureDir(dir); err != nil {
		return nil, err
	}
	path := lockPath(name, dir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// flock locks belong to the open file description, so a second open of
	// the same file conflicts even inside this process.
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		_ = f.Close()
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return &Lock{release: func() error {
		uerr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		if cerr := f.Close(); cerr != nil {
			return cerr
		}
		return uerr
	}}, nil
}
