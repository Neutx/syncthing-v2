//go:build darwin

package autostart

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// DefaultRoots returns ~/Library/LaunchAgents, SyncThing V2's log directory
// and launchctl for the current user.
func DefaultRoots() (Roots, error) {
	home, err := osutil.HomeDir()
	if err != nil {
		return Roots{}, err
	}
	return Roots{
		LaunchAgents: filepath.Join(home, "Library", "LaunchAgents"),
		LogDir:       filepath.Join(home, "Library", "Logs", brand.AppDirName()),
		UID:          os.Getuid(),
		Run:          runCommand,
	}, nil
}

// New returns a Manager for r. On macOS it writes LaunchAgent plists.
func New(r Roots) *Manager { return &Manager{impl: newLaunchd(r)} }

// runCommand is the production Roots.Run: launchctl, bounded to 15 s.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return osutil.Output(ctx, 15*time.Second, name, args...)
}
