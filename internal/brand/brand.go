// Package brand holds every user-visible name, slug and identifier of
// SyncThing V2, so that a rename touches one file.
package brand

import (
	"runtime"
	"runtime/debug"
)

const (
	// DisplayName is the product name shown to users.
	DisplayName = "SyncThing V2"
	// BinaryName is the executable name without an extension.
	BinaryName = "stv2"
	// RepoOwner and RepoName identify the GitHub repository.
	RepoOwner = "Neutx"
	RepoName  = "syncthing-v2"
	// Module is the Go module path.
	Module = "github.com/Neutx/syncthing-v2"
	// BundleID is the macOS bundle identifier and the reverse-DNS prefix.
	BundleID = "io.github.neutx.syncthingv2"
	// Publisher is the publisher name used in installer metadata and the licence.
	Publisher = "SyncThing V2 contributors"
	// MutexName is the Windows single-instance mutex. It must differ from the
	// legacy prototype's mutex ("SyncthingTraySingleInstance").
	MutexName = `Local\SyncThingV2.Tray`
	// MinSyncthing is the oldest Syncthing release SyncThing V2 supports.
	MinSyncthing = "v1.27.0"
	// BootstrapSyncthing is the upstream release downloaded when Syncthing is missing.
	BootstrapSyncthing = "v2.1.5"

	// Disclaimer appears verbatim in the README footer, the About surfaces and `stv2 version`.
	Disclaimer = `SyncThing V2 is an independent open-source project. It is not affiliated with, endorsed by, or sponsored by the Syncthing Foundation or Tailscale Inc. "Syncthing" is a trademark of the Syncthing Foundation. "Tailscale" is a trademark of Tailscale Inc. SyncThing V2 does not use the Syncthing logo.`
)

// Version is injected at link time with
// -X github.com/Neutx/syncthing-v2/internal/brand.Version=<x.y.z>.
var Version = "0.0.0-dev"

// RepoURL is the public repository address.
const RepoURL = "https://github.com/" + RepoOwner + "/" + RepoName

// ExeName returns the executable file name for the running OS.
func ExeName() string { return exeName(runtime.GOOS) }

func exeName(goos string) string {
	if goos == "windows" {
		return BinaryName + ".exe"
	}
	return BinaryName
}

// AppDirName returns the per-user data directory name for the running OS.
func AppDirName() string { return appDirName(runtime.GOOS) }

func appDirName(goos string) string {
	if goos == "linux" {
		return "syncthing-v2"
	}
	return "SyncThingV2"
}

// AtLogin returns the per-OS wording used in "Start ... <AtLogin>" menu items.
func AtLogin() string { return atLogin(runtime.GOOS) }

func atLogin(goos string) string {
	switch goos {
	case "windows":
		return "with Windows"
	case "darwin":
		return "at login"
	default:
		return "on login"
	}
}

// UserAgent is the HTTP User-Agent for outbound requests (release checks, downloads).
func UserAgent() string { return BinaryName + "/" + Version }

// Commit returns the VCS revision recorded by the Go toolchain (-buildvcs),
// shortened to 12 characters, with a "-dirty" suffix for modified trees.
// It returns "unknown" when the binary carries no VCS information.
func Commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return commitFrom(info.Settings)
}

func commitFrom(settings []debug.BuildSetting) string {
	rev, dirty := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
