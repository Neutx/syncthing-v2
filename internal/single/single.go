// Package single enforces one running tray per user session and publishes
// how to reach it: instance.json (mode 0600, in the app data directory)
// holds the dashboard server's port, the tray's PID and the control token
// that a second launch or the installer uses for /api/show and /api/quit.
package single

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// ErrAlreadyRunning is returned by Acquire when another instance holds the lock.
var ErrAlreadyRunning = errors.New("another SyncThing V2 instance is already running")

// Lock is a held single-instance lock. It is released by Release or when the
// process exits.
type Lock struct {
	once    sync.Once
	release func() error
	err     error
}

// Acquire takes the single-instance lock called name. On Windows it is a
// named mutex (name is the mutex name, e.g. brand.MutexName) and dir is not
// used. On Unix it is an exclusive flock on dir/<base of name>.lock.
func Acquire(name, dir string) (*Lock, error) {
	if name == "" {
		return nil, errors.New("single: empty lock name")
	}
	return acquire(name, dir)
}

// Release frees the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	l.once.Do(func() { l.err = l.release() })
	return l.err
}

// InstanceFile is the control file name inside the app data directory.
const InstanceFile = "instance.json"

// Instance describes the running tray.
type Instance struct {
	Port  int    `json:"port"`
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func (in Instance) validate() error {
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("instance: invalid port %d", in.Port)
	}
	if in.PID < 1 {
		return fmt.Errorf("instance: invalid pid %d", in.PID)
	}
	if len(in.Token) < 16 {
		return errors.New("instance: control token too short")
	}
	return nil
}

// WriteInstance atomically writes dir/instance.json with mode 0600.
func WriteInstance(dir string, in Instance) error {
	if err := in.validate(); err != nil {
		return err
	}
	if err := osutil.EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return osutil.WriteFileAtomic(filepath.Join(dir, InstanceFile), data, 0o600)
}

// ReadInstance reads and validates dir/instance.json.
func ReadInstance(dir string) (Instance, error) {
	var in Instance
	data, err := os.ReadFile(filepath.Join(dir, InstanceFile))
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return Instance{}, fmt.Errorf("instance: %w", err)
	}
	if err := in.validate(); err != nil {
		return Instance{}, err
	}
	return in, nil
}

// RemoveInstance deletes dir/instance.json if it still belongs to pid, so an
// exiting tray never removes the file of a newer instance. A missing file is
// not an error.
func RemoveInstance(dir string, pid int) error {
	in, err := ReadInstance(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && in.PID != pid {
		return nil
	}
	// Unreadable or ours: remove it.
	if err := os.Remove(filepath.Join(dir, InstanceFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Signal asks the running instance described by dir/instance.json to act:
// action is "show" (open the dashboard) or "quit". It POSTs to
// http://127.0.0.1:<port>/api/<action> with the bearer control token.
func Signal(ctx context.Context, dir, action string) error {
	if action != "show" && action != "quit" {
		return fmt.Errorf("single: unknown action %q", action)
	}
	in, err := ReadInstance(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := "http://127.0.0.1:" + strconv.Itoa(in.Port) + "/api/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+in.Token)
	req.Header.Set("X-STV2", "1")
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil}, // loopback only; never through a proxy
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("single: %s: %w", action, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("single: %s: running instance answered %s", action, resp.Status)
	}
	return nil
}
