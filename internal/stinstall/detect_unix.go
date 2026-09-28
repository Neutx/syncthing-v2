//go:build !windows

package stinstall

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// runningSyncthing returns the executable paths of this user's running
// syncthing processes: from /proc where it exists (Linux; only processes
// whose exe link this user may read), otherwise from
// `ps -axo pid=,uid=,comm=` (macOS prints the full executable path in comm),
// keeping only rows whose uid is this user's.
func runningSyncthing(ctx context.Context) []string {
	if fi, err := os.Stat("/proc/self/exe"); err == nil && fi != nil {
		return procSyncthing(ctx, "/proc", isRegular)
	}
	out, err := osutil.Output(ctx, 5*time.Second, "ps", "-axo", "pid=,uid=,comm=")
	if err != nil {
		return nil
	}
	return parsePS(out, os.Getuid())
}

// trustedBin reports whether p is a regular file that is safe to run as this
// user: owned by root or by this user, and writable by no one else. Anything
// else could be swapped by another local user between detection and every
// later start, so it is never adopted.
func trustedBin(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && ownedSafely(fi, os.Getuid())
}

func ownedSafely(fi os.FileInfo, uid int) bool {
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == 0 || int64(st.Uid) == int64(uid)
}
