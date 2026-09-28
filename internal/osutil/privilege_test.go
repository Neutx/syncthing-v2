package osutil

import (
	"errors"
	"strings"
	"testing"
)

func TestPrivilegeError(t *testing.T) {
	if err := privilegeError(false, ""); err != nil {
		t.Errorf("unprivileged: %v", err)
	}
	if err := privilegeError(true, "1"); err != nil {
		t.Errorf("privileged with the override: %v", err)
	}
	for _, override := range []string{"", "0", "true", " 1"} {
		err := privilegeError(true, override)
		if !errors.Is(err, ErrPrivileged) {
			t.Fatalf("privileged, override %q: err = %v, want ErrPrivileged", override, err)
		}
		if !strings.Contains(err.Error(), AllowPrivilegedEnv+"=1") {
			t.Errorf("error %q does not name the override %s=1", err, AllowPrivilegedEnv)
		}
	}
}

func TestCheckUnprivilegedHonoursOverride(t *testing.T) {
	t.Setenv(AllowPrivilegedEnv, "1")
	if err := CheckUnprivileged(); err != nil {
		t.Errorf("CheckUnprivileged with %s=1: %v", AllowPrivilegedEnv, err)
	}
	t.Setenv(AllowPrivilegedEnv, "")
	if err := CheckUnprivileged(); (err != nil) != isPrivileged() {
		t.Errorf("CheckUnprivileged = %v, but isPrivileged = %v", err, isPrivileged())
	}
}
