package stinstall

import (
	"fmt"
	"path/filepath"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// Start launches Syncthing as a detached process that is never a child the
// tray waits on, so quitting or crashing the tray never stops sync (§5.2).
// With no extraArgs it uses the per-OS serve flags from ServeArgs; on macOS
// and Linux it first asks launchd or systemd to start SyncThing V2's own
// Syncthing entry when one is registered. extraArgs (for example --home for
// a sandbox instance) are appended to the serve flags and always start the
// binary directly. An elevated process (administrator on Windows, root on
// macOS and Linux) refuses, so the network-facing daemon never inherits
// administrator rights (osutil.CheckUnprivileged).
func Start(bin string, extraArgs ...string) error {
	if err := osutil.CheckUnprivileged(); err != nil {
		return fmt.Errorf("start syncthing: %w", err)
	}
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("start syncthing: %q is not an absolute path", bin)
	}
	if !isRegular(bin) {
		return fmt.Errorf("start syncthing: %s does not exist", bin)
	}
	if err := start(bin, extraArgs); err != nil {
		return fmt.Errorf("start syncthing: %w", err)
	}
	return nil
}
