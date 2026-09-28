//go:build !windows

package stinstall

import (
	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// ServeArgs are the flags Syncthing is started with when nothing supervises
// it. --no-restart is left out here: without launchd or systemd, Syncthing's
// own monitor process is what restarts it after an upgrade or a crash. The
// launchd and systemd entries (package autostart) add --no-restart because
// the service manager restarts it instead.
func ServeArgs() []string { return []string{"serve", "--no-browser"} }

// start prefers the service manager for SyncThing V2's own Syncthing entry
// (launchd LaunchAgent, systemd user unit). When there is none, or the
// service manager fails, it starts the binary in a new session (Setsid).
func start(bin string, extra []string) error {
	if len(extra) == 0 {
		if registered, err := autostart.StartService(autostart.Syncthing); registered && err == nil {
			return nil
		}
	}
	_, err := osutil.StartDetached(bin, append(ServeArgs(), extra...)...)
	return err
}
