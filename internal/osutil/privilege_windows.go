//go:build windows

package osutil

import "golang.org/x/sys/windows"

// AllowPrivilegedEnv is the environment variable that, set to "1", lets
// SyncThing V2 run elevated anyway (CheckUnprivileged).
const AllowPrivilegedEnv = "STV2_ALLOW_ADMIN"

const (
	privilegedAs  = "as administrator"
	privilegeHint = `run it from a normal (non-administrator) PowerShell or without "Run as administrator"`
)

func isPrivileged() bool { return windows.GetCurrentProcessToken().IsElevated() }
