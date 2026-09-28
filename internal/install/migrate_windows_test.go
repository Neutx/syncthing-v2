//go:build windows

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// terminateProcess re-checks the full image path, so a process that is not
// the legacy prototype (here the test binary itself) is never terminated.
func TestTerminateProcessChecksPath(t *testing.T) {
	notLegacy := filepath.Join(t.TempDir(), "Programs", "Syncthing", "tray", LegacyProcess)
	for _, exe := range []string{notLegacy, ""} {
		err := terminateProcess(os.Getpid(), exe)
		if err == nil || !strings.Contains(err.Error(), "not the legacy tray") {
			t.Fatalf("terminateProcess(self, %q) = %v, want a refusal", exe, err)
		}
	}
}

// The process list carries the full path of processes named like the
// legacy tray, and only of those.
func TestListProcessesPaths(t *testing.T) {
	list, err := listProcesses()
	if err != nil {
		t.Fatal(err)
	}
	self := false
	for _, p := range list {
		if p.PID == os.Getpid() {
			self = true
			if p.Path != "" {
				t.Errorf("path read for a process not named %s: %+v", LegacyProcess, p)
			}
		}
	}
	if !self {
		t.Error("the test process is missing from the process list")
	}
}
