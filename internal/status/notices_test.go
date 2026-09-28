package status

import (
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
)

func TestNoticeFor(t *testing.T) {
	const (
		down   = model.StateDown
		unauth = model.StateUnauthorized
		errSt  = model.StateError
		paused = model.StatePaused
		sync   = model.StateSyncing
		scan   = model.StateScanning
		nopeer = model.StateNoPeer
		insync = model.StateInSync
	)
	snap := model.Snapshot{FolderLine: "3 error(s)", Detail: "some detail"}
	cases := []struct {
		prev, next  model.State
		title, body string
	}{
		{insync, nopeer, "Syncthing disconnected", "No devices are connected."},
		{sync, nopeer, "Syncthing disconnected", "No devices are connected."},
		{down, nopeer, "", ""},
		{unauth, nopeer, "", ""},
		{insync, down, "Syncthing stopped", "The Syncthing process is not running."},
		{unauth, down, "Syncthing stopped", "The Syncthing process is not running."},
		{insync, unauth, "API key rejected", "some detail"},
		{insync, errSt, "Syncthing error", "3 error(s)"},
		{sync, insync, "Syncthing in sync", "All files are up to date."},
		{nopeer, insync, "Syncthing in sync", "All files are up to date."},
		{scan, insync, "", ""},
		{down, insync, "", ""},
		{paused, insync, "", ""},
		{insync, sync, "", ""},
		{insync, scan, "", ""},
		{insync, paused, "", ""},
		{insync, insync, "", ""},
		{nopeer, nopeer, "", ""},
		{down, down, "", ""},
	}
	for _, c := range cases {
		title, body, ok := NoticeFor(c.prev, c.next, snap)
		if ok != (c.title != "") || title != c.title || body != c.body {
			t.Errorf("NoticeFor(%s → %s) = %q, %q, %v; want %q, %q", Label(c.prev), Label(c.next), title, body, ok, c.title, c.body)
		}
	}

	// Fallbacks when the snapshot lacks the usual text.
	if _, body, _ := NoticeFor(insync, errSt, model.Snapshot{Detail: detailBadResponse}); body != detailBadResponse {
		t.Errorf("error without folder line: body %q", body)
	}
	if _, body, _ := NoticeFor(insync, unauth, model.Snapshot{}); body != detailUnauthorized {
		t.Errorf("unauthorized without detail: body %q", body)
	}
}
