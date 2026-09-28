//go:build windows

package stinstall

import "github.com/Neutx/syncthing-v2/internal/osutil"

// ServeArgs are the flags Syncthing is started with when nothing supervises
// it: no console window and no browser.
func ServeArgs() []string { return []string{"serve", "--no-console", "--no-browser"} }

// start runs Syncthing with CREATE_NO_WINDOW | DETACHED_PROCESS |
// CREATE_NEW_PROCESS_GROUP (osutil.StartDetached).
func start(bin string, extra []string) error {
	_, err := osutil.StartDetached(bin, append(ServeArgs(), extra...)...)
	return err
}
