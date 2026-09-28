package osutil

import (
	"errors"
	"fmt"
	"os"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// ErrPrivileged is returned (wrapped) by CheckUnprivileged when this process
// runs with administrator rights and the override is not set.
var ErrPrivileged = errors.New("refusing to run with administrator rights")

// CheckUnprivileged returns an error wrapping ErrPrivileged when this process
// runs elevated (an elevated token on Windows, root on macOS and Linux),
// unless the environment variable AllowPrivilegedEnv is "1". SyncThing V2 is
// a per-user app: run elevated, the Syncthing daemon it starts (which serves
// remote peers), the dashboard and every URL or folder it opens would all
// have administrator rights, and its files would land in that account.
func CheckUnprivileged() error {
	return privilegeError(isPrivileged(), os.Getenv(AllowPrivilegedEnv))
}

func privilegeError(privileged bool, override string) error {
	if !privileged || override == "1" {
		return nil
	}
	return fmt.Errorf("%w: %s is a per-user app and must not run %s; %s, or set %s=1 to override", ErrPrivileged,
		brand.DisplayName, privilegedAs, privilegeHint, AllowPrivilegedEnv)
}
