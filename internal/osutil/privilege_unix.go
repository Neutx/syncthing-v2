//go:build !windows

package osutil

import "os"

// AllowPrivilegedEnv is the environment variable that, set to "1", lets
// SyncThing V2 run as root anyway (CheckUnprivileged). install.sh sets it
// for --allow-root.
const AllowPrivilegedEnv = "STV2_ALLOW_ROOT"

const (
	privilegedAs  = "as root"
	privilegeHint = "run it as your normal user, without sudo"
)

func isPrivileged() bool { return os.Geteuid() == 0 }
