package brand

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestAtLoginPerOS(t *testing.T) {
	cases := map[string]string{
		"windows": "with Windows",
		"darwin":  "at login",
		"linux":   "on login",
	}
	for goos, want := range cases {
		if got := atLogin(goos); got != want {
			t.Errorf("atLogin(%q) = %q, want %q", goos, got, want)
		}
	}
	if got, want := AtLogin(), atLogin(runtime.GOOS); got != want {
		t.Errorf("AtLogin() = %q, want %q", got, want)
	}
}

func TestAppDirNamePerOS(t *testing.T) {
	cases := map[string]string{
		"windows": "SyncThingV2",
		"darwin":  "SyncThingV2",
		"linux":   "syncthing-v2",
	}
	for goos, want := range cases {
		if got := appDirName(goos); got != want {
			t.Errorf("appDirName(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestExeName(t *testing.T) {
	if got := exeName("windows"); got != "stv2.exe" {
		t.Errorf("exeName(windows) = %q", got)
	}
	for _, goos := range []string{"darwin", "linux"} {
		if got := exeName(goos); got != "stv2" {
			t.Errorf("exeName(%s) = %q", goos, got)
		}
	}
}

func TestConstants(t *testing.T) {
	if MutexName == `SyncthingTraySingleInstance` || !strings.HasPrefix(MutexName, `Local\`) {
		t.Errorf("MutexName %q must be a Local\\ name distinct from the prototype's", MutexName)
	}
	if Module != "github.com/"+RepoOwner+"/"+RepoName {
		t.Errorf("Module %q does not match the repository", Module)
	}
	if RepoURL != "https://github.com/Neutx/syncthing-v2" {
		t.Errorf("RepoURL = %q", RepoURL)
	}
	if Version == "" {
		t.Error("Version must never be empty")
	}
	if UserAgent() != "stv2/"+Version {
		t.Errorf("UserAgent() = %q", UserAgent())
	}
}

func TestDisclaimerVerbatim(t *testing.T) {
	const want = `SyncThing V2 is an independent open-source project. It is not affiliated with, endorsed by, or sponsored by the Syncthing Foundation or Tailscale Inc. "Syncthing" is a trademark of the Syncthing Foundation. "Tailscale" is a trademark of Tailscale Inc. SyncThing V2 does not use the Syncthing logo.`
	if Disclaimer != want {
		t.Errorf("Disclaimer drifted from the spec text:\n got %q\nwant %q", Disclaimer, want)
	}
}

func TestCommitFrom(t *testing.T) {
	cases := []struct {
		settings []debug.BuildSetting
		want     string
	}{
		{nil, "unknown"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123"}}, "0123456789ab"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "true"}}, "abc-dirty"},
		{[]debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}, "unknown"},
	}
	for _, c := range cases {
		if got := commitFrom(c.settings); got != c.want {
			t.Errorf("commitFrom(%v) = %q, want %q", c.settings, got, c.want)
		}
	}
	if Commit() == "" {
		t.Error("Commit() must never be empty")
	}
}
