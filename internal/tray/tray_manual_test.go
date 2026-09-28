//go:build windows

package tray

import (
	"flag"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
)

var manual = flag.Bool("manual", false, "run the interactive tray check (TestManualTray)")

// TestManualTray is an interactive check of the real tray:
//
//	go test ./internal/tray -run TestManualTray -manual -v
//
// It shows the icon (cycling through every state), a four-line tooltip and
// the full menu, then asks for a left click, a balloon click and the Exit
// menu item. All data is synthetic.
func TestManualTray(t *testing.T) {
	if !*manual {
		t.Skip("interactive; run with -manual")
	}
	const wait = 2 * time.Minute
	tr := newTray().(*winTray)
	events := make(chan string, 16)
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Run(func() { close(ready) }, func() { events <- "click" }, func(id string) { events <- "menu:" + id })
	}()
	<-ready

	snap := model.Snapshot{
		State: model.StateSyncing, Label: "Syncing", Moving: true, Pct: 40,
		PeerLine: "alpha (Tailscale), beta (local network)", FolderLine: "40% - 1.5 GB left",
		ETA: "4m", SpeedLine: "v 6.2 MB/s  ^ 40 KB/s", GUIURL: "http://127.0.0.1:18384/",
		Folders:         []model.Folder{{ID: "f1", Label: "Photos & Video"}, {ID: "f2", Label: "Notes"}},
		Startup:         model.Startup{Syncthing: true, Tray: true, SyncthingManagedByUs: true},
		UpdateAvailable: "v9.9.9",
	}
	tr.SetTooltip(Tooltip(snap))
	tr.SetMenu(BuildMenu(snap))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		states := []model.State{model.StateSyncing, model.StateInSync, model.StateScanning, model.StatePaused,
			model.StateNoPeer, model.StateError, model.StateUnauthorized, model.StateDown}
		for i := 0; ; i++ {
			tr.SetIcon(states[i%len(states)], (i*10)%100)
			select {
			case <-stop:
				return
			case <-time.After(1200 * time.Millisecond):
			}
		}
	}()

	await := func(prompt string, ok func(string) bool) {
		t.Helper()
		t.Log(prompt)
		deadline := time.After(wait)
		for {
			select {
			case e := <-events:
				t.Logf("event %q", e)
				if ok(e) {
					return
				}
			case <-deadline:
				t.Fatalf("timed out: %s", prompt)
			}
		}
	}

	await("Hover the icon (expect 4 tooltip lines), then LEFT-CLICK it.", func(e string) bool { return e == "click" })

	balloonClicked := make(chan struct{}, 1)
	if !tr.BalloonWithClick("SyncThing V2 test", "Click this notification.", func() { balloonClicked <- struct{}{} }) {
		t.Fatal("balloon was not queued")
	}
	t.Log("CLICK the notification that just appeared.")
	select {
	case <-balloonClicked:
	case <-time.After(wait):
		t.Fatal("timed out waiting for the balloon click")
	}

	await("RIGHT-CLICK the icon, check the menu and submenu, then choose Exit.", func(e string) bool { return e == "menu:"+IDExit })
	tr.Quit()
	<-done
}
