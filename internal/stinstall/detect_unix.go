//go:build !windows

package stinstall

import (
	"context"
	"os"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// runningSyncthing returns the executable paths of running syncthing
// processes: from /proc where it exists (Linux; only processes whose exe
// link this user may read), otherwise from `ps -axo pid=,comm=` (macOS
// prints the full executable path in comm).
func runningSyncthing(ctx context.Context) []string {
	if fi, err := os.Stat("/proc/self/exe"); err == nil && fi != nil {
		return procSyncthing(ctx, "/proc")
	}
	out, err := osutil.Output(ctx, 5*time.Second, "ps", "-axo", "pid=,comm=")
	if err != nil {
		return nil
	}
	return parsePS(out)
}
