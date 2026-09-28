//go:build linux

package autostart

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// DistroUnitDirs are where distribution packages install systemd user units.
var DistroUnitDirs = []string{"/usr/lib/systemd/user", "/lib/systemd/user", "/usr/local/lib/systemd/user"}

// DefaultRoots returns $XDG_CONFIG_HOME (default ~/.config), the distro
// unit directories and systemctl, when it is installed.
func DefaultRoots() (Roots, error) {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(cfg) { // the XDG spec says relative values are invalid
		home, err := osutil.HomeDir()
		if err != nil {
			return Roots{}, err
		}
		cfg = filepath.Join(home, ".config")
	}
	r := Roots{ConfigHome: cfg, UnitDirs: DistroUnitDirs}
	if u, err := user.Current(); err == nil {
		r.User = u.Username
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		r.Run = runCommand
	}
	return r, nil
}

// New returns a Manager for r. On Linux it uses systemd user units for
// Syncthing when a user manager runs, and XDG autostart files otherwise.
func New(r Roots) *Manager { return &Manager{impl: newXDG(r)} }

// runCommand is the production Roots.Run: systemctl, bounded to 15 s.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return osutil.Output(ctx, 15*time.Second, name, args...)
}
