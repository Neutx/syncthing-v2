package tray

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// render prints a menu model one item per line, for golden comparison.
func render(items []MenuItem) string {
	var b strings.Builder
	var walk func([]MenuItem, string)
	walk = func(items []MenuItem, indent string) {
		for _, it := range items {
			if it.ID == Separator {
				b.WriteString(indent + "----\n")
				continue
			}
			flags := ""
			if !it.Enabled {
				flags += " disabled"
			}
			if it.Checked {
				flags += " checked"
			}
			if !it.Visible {
				flags += " hidden"
			}
			fmt.Fprintf(&b, "%s%s %q%s\n", indent, it.ID, it.Text, flags)
			walk(it.Children, indent+"  ")
		}
	}
	walk(items, "")
	return b.String()
}

func TestBuildMenuGoldens(t *testing.T) {
	cases := []struct {
		name string
		s    model.Snapshot
		want string
	}{
		{
			name: "before first poll",
			s:    model.Snapshot{},
			want: `info-state "Not running" disabled
info-transfer "Transfer: n/a" disabled
info-startup "Startup: Syncthing OFF | Tray OFF" disabled
----
open-status "Open Status Window"
pair "Pair Devices…"
open-webui "Open Web UI" disabled
open-folder "Open Sync Folder" disabled
----
rescan "Rescan All" disabled
pause "Pause All" disabled
restart "Restart Syncthing" disabled
start-syncthing "Start Syncthing"
----
autostart-tray "Start SyncThing V2 with Windows"
autostart-syncthing "Start Syncthing with Windows"
----
update "Update available…" hidden
about "About SyncThing V2"
exit "Exit"
`,
		},
		{
			name: "in sync, one folder, both autostarts managed",
			s: model.Snapshot{
				State: model.StateInSync, Label: "In sync",
				FolderLine: "Up to date", SpeedLine: "idle",
				GUIURL:  "http://127.0.0.1:8384/",
				Folders: []model.Folder{{ID: "abcd-1234", Label: "Documents", Path: "/synthetic/Documents"}},
				Startup: model.Startup{Syncthing: true, Tray: true, SyncthingManagedByUs: true},
			},
			want: `info-state "In sync  -  Up to date" disabled
info-transfer "Transfer: idle" disabled
info-startup "Startup: Syncthing ON | Tray ON" disabled
----
open-status "Open Status Window"
pair "Pair Devices…"
open-webui "Open Web UI"
open-folder:abcd-1234 "Open Sync Folder"
----
rescan "Rescan All"
pause "Pause All"
restart "Restart Syncthing"
start-syncthing "Start Syncthing" hidden
----
autostart-tray "Start SyncThing V2 with Windows" checked
autostart-syncthing "Start Syncthing with Windows" checked
----
update "Update available…" hidden
about "About SyncThing V2"
exit "Exit"
`,
		},
		{
			name: "syncing, several folders, external autostart, update",
			s: model.Snapshot{
				State: model.StateSyncing, Label: "Syncing",
				FolderLine: "42% - 1.2 GB left", SpeedLine: "v 3.4 MB/s  ^ 12 KB/s", ETA: "6m", Moving: true,
				GUIURL: "http://127.0.0.1:8384/",
				Folders: []model.Folder{
					{ID: "photos-01", Label: "Photos & Video"},
					{ID: "code-02", Label: ""},
					{ID: "notes-03", Label: "Notes"},
				},
				Startup:         model.Startup{Syncthing: true, Tray: false, SyncthingManagedByUs: false},
				UpdateAvailable: "v1.2.3",
			},
			want: `info-state "Syncing  -  42% - 1.2 GB left" disabled
info-transfer "Transfer: v 3.4 MB/s  ^ 12 KB/s   (ETA 6m)" disabled
info-startup "Startup: Syncthing ON | Tray OFF" disabled
----
open-status "Open Status Window"
pair "Pair Devices…"
open-webui "Open Web UI"
open-folder "Open Sync Folder"
  open-folder:photos-01 "Photos & Video"
  open-folder:code-02 "code-02"
  open-folder:notes-03 "Notes"
----
rescan "Rescan All"
pause "Pause All"
restart "Restart Syncthing"
start-syncthing "Start Syncthing" hidden
----
autostart-tray "Start SyncThing V2 with Windows"
autostart-syncthing "Start Syncthing with Windows" disabled checked
----
update "Update available (v1.2.3)…"
about "About SyncThing V2"
exit "Exit"
`,
		},
		{
			name: "paused offers resume",
			s: model.Snapshot{
				State: model.StatePaused, Label: "Paused", FolderLine: "Paused",
				GUIURL:  "http://127.0.0.1:8384/",
				Folders: []model.Folder{{ID: "a", Label: "A", Paused: true}},
			},
			want: `info-state "Paused  -  Paused" disabled
info-transfer "Transfer: n/a" disabled
info-startup "Startup: Syncthing OFF | Tray OFF" disabled
----
open-status "Open Status Window"
pair "Pair Devices…"
open-webui "Open Web UI"
open-folder:a "Open Sync Folder"
----
rescan "Rescan All"
resume "Resume All"
restart "Restart Syncthing"
start-syncthing "Start Syncthing" hidden
----
autostart-tray "Start SyncThing V2 with Windows"
autostart-syncthing "Start Syncthing with Windows"
----
update "Update available…" hidden
about "About SyncThing V2"
exit "Exit"
`,
		},
		{
			name: "unauthorized does not offer start",
			s: model.Snapshot{
				State: model.StateUnauthorized, Label: "API key rejected",
				GUIURL: "http://127.0.0.1:8384/",
			},
			want: `info-state "API key rejected" disabled
info-transfer "Transfer: n/a" disabled
info-startup "Startup: Syncthing OFF | Tray OFF" disabled
----
open-status "Open Status Window"
pair "Pair Devices…"
open-webui "Open Web UI"
open-folder "Open Sync Folder" disabled
----
rescan "Rescan All" disabled
pause "Pause All" disabled
restart "Restart Syncthing" disabled
start-syncthing "Start Syncthing" hidden
----
autostart-tray "Start SyncThing V2 with Windows"
autostart-syncthing "Start Syncthing with Windows"
----
update "Update available…" hidden
about "About SyncThing V2"
exit "Exit"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := render(buildMenu(tc.s, "with Windows"))
			if got != tc.want {
				t.Errorf("menu mismatch\n--- got\n%s--- want\n%s", got, tc.want)
			}
		})
	}
}

func TestBuildMenuStableShape(t *testing.T) {
	// The fyne tray updates in place while the shape holds; state changes
	// that do not add or remove folders or the update entry must keep it.
	base := model.Snapshot{State: model.StateInSync, Folders: []model.Folder{{ID: "a"}}}
	down := base
	down.State = model.StateDown
	paused := base
	paused.State = model.StatePaused
	a, b := buildMenu(base, "on login"), buildMenu(down, "on login")
	if len(a) != len(b) {
		t.Fatalf("menu length changed with state: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("item %d ID %q vs %q", i, a[i].ID, b[i].ID)
		}
	}
	if len(buildMenu(paused, "on login")) != len(a) {
		t.Error("pause/resume must occupy the same slot")
	}
}

func TestAtLoginWording(t *testing.T) {
	for _, w := range []string{"with Windows", "at login", "on login"} {
		m := buildMenu(model.Snapshot{}, w)
		var found int
		for _, it := range m {
			if it.ID == IDAutostartTray && it.Text == "Start SyncThing V2 "+w {
				found++
			}
			if it.ID == IDAutostartSyncthing && it.Text == "Start Syncthing "+w {
				found++
			}
		}
		if found != 2 {
			t.Errorf("wording %q not applied to both autostart items", w)
		}
	}
	if got := BuildMenu(model.Snapshot{}); len(got) != len(buildMenu(model.Snapshot{}, "x")) {
		t.Error("BuildMenu and buildMenu disagree")
	}
}

func TestFolderID(t *testing.T) {
	cases := map[string]struct {
		id string
		ok bool
	}{
		"open-folder:abc":     {"abc", true},
		"open-folder:a:b":     {"a:b", true},
		"open-folder:":        {"", false},
		"open-folder":         {"", false},
		"rescan":              {"", false},
		"x-open-folder:abc":   {"", false},
		FolderPrefix + "f-01": {"f-01", true},
	}
	for in, want := range cases {
		id, ok := FolderID(in)
		if id != want.id || ok != want.ok {
			t.Errorf("FolderID(%q) = %q, %v; want %q, %v", in, id, ok, want.id, want.ok)
		}
	}
}

func TestTooltipGoldens(t *testing.T) {
	cases := []struct {
		name string
		s    model.Snapshot
		want string
	}{
		{
			name: "before first poll",
			s:    model.Snapshot{},
			want: "SyncThing V2 - Not running",
		},
		{
			name: "in sync",
			s: model.Snapshot{State: model.StateInSync, Label: "In sync",
				PeerLine: "alpha (Tailscale)", FolderLine: "Up to date", SpeedLine: "idle"},
			want: "SyncThing V2 - In sync\nalpha (Tailscale)\nUp to date",
		},
		{
			name: "syncing and moving has four lines",
			s: model.Snapshot{State: model.StateSyncing, Label: "Syncing", Moving: true,
				PeerLine: "alpha (Tailscale)", FolderLine: "42% - 1.2 GB left", ETA: "6m",
				SpeedLine: "v 3.4 MB/s  ^ 12 KB/s"},
			want: "SyncThing V2 - Syncing\nalpha (Tailscale)\n42% - 1.2 GB left  ETA 6m\nv 3.4 MB/s  ^ 12 KB/s",
		},
		{
			name: "syncing and moving without an ETA",
			s: model.Snapshot{State: model.StateSyncing, Label: "Syncing", Moving: true,
				PeerLine: "alpha (relay)", FolderLine: "10% - 9.0 GB left", SpeedLine: "v 1.0 KB/s  ^ 0 KB/s"},
			want: "SyncThing V2 - Syncing\nalpha (relay)\n10% - 9.0 GB left\nv 1.0 KB/s  ^ 0 KB/s",
		},
		{
			name: "syncing but idle keeps three lines",
			s: model.Snapshot{State: model.StateSyncing, Label: "Syncing", Moving: false,
				PeerLine: "alpha (Tailscale)", FolderLine: "99% - 12 KB left", ETA: "1s", SpeedLine: "idle"},
			want: "SyncThing V2 - Syncing\nalpha (Tailscale)\n99% - 12 KB left",
		},
		{
			name: "moving outside syncing keeps three lines",
			s: model.Snapshot{State: model.StateScanning, Label: "Checking", Moving: true,
				PeerLine: "alpha (local network)", FolderLine: "Checking for changes", SpeedLine: "v 5.0 KB/s  ^ 0 KB/s"},
			want: "SyncThing V2 - Checking\nalpha (local network)\nChecking for changes",
		},
		{
			name: "disconnected with need",
			s: model.Snapshot{State: model.StateNoPeer, Label: "Disconnected",
				PeerLine: "No devices connected", FolderLine: "37% - 800 MB left"},
			want: "SyncThing V2 - Disconnected\nNo devices connected\n37% - 800 MB left",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Tooltip(tc.s); got != tc.want {
				t.Errorf("Tooltip =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestTooltipCap(t *testing.T) {
	peers := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		peers = append(peers, fmt.Sprintf("device-%02d (Tailscale)", i))
	}
	s := model.Snapshot{State: model.StateSyncing, Label: "Syncing", Moving: true,
		PeerLine: strings.Join(peers, ", "), FolderLine: "42% - 1.2 GB left", ETA: "6m",
		SpeedLine: "v 3.4 MB/s  ^ 12 KB/s"}
	got := Tooltip(s)
	if n := len(utf16.Encode([]rune(got))); n != TooltipMax {
		t.Errorf("capped tooltip has %d UTF-16 units, want %d", n, TooltipMax)
	}
	if !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "SyncThing V2 - Syncing\ndevice-00 (Tailscale), ") {
		t.Errorf("capped tooltip %q", got)
	}
	full := "SyncThing V2 - Syncing\n" + s.PeerLine
	if got != full[:TooltipMax-3]+"..." {
		t.Errorf("cap cut at the wrong place: %q", got)
	}

	// Exactly at the limit: unchanged.
	exact := model.Snapshot{Label: "In sync", PeerLine: strings.Repeat("p", TooltipMax-len("SyncThing V2 - In sync\n"))}
	if got := Tooltip(exact); len(got) != TooltipMax || strings.HasSuffix(got, "...") {
		t.Errorf("tooltip at the limit was changed: %d %q", len(got), got)
	}
}

func TestTooltipCapUnicode(t *testing.T) {
	// Emoji take two UTF-16 units; the cut must not split a surrogate pair
	// and the result must stay valid UTF-8 within 127 units.
	for pad := 0; pad < 4; pad++ {
		s := model.Snapshot{Label: "In sync", PeerLine: strings.Repeat("x", pad) + strings.Repeat("\U0001F4BB", 80)}
		got := Tooltip(s)
		if !utf8.ValidString(got) {
			t.Fatalf("pad %d: invalid UTF-8 %q", pad, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("pad %d: split surrogate %q", pad, got)
		}
		if n := len(utf16.Encode([]rune(got))); n > TooltipMax {
			t.Fatalf("pad %d: %d UTF-16 units", pad, n)
		}
		if !strings.HasSuffix(got, "...") {
			t.Fatalf("pad %d: missing ellipsis", pad)
		}
	}
	// Non-ASCII BMP text counts one unit per character, not per byte.
	s := model.Snapshot{Label: "In sync", PeerLine: strings.Repeat("é", 100)}
	if got := Tooltip(s); strings.HasSuffix(got, "...") {
		t.Errorf("123 units must not be cut: %q", got)
	}
}

func TestLabelFallback(t *testing.T) {
	want := map[model.State]string{
		model.StateDown:         "Not running",
		model.StateUnauthorized: "API key rejected",
		model.StateError:        "Error",
		model.StatePaused:       "Paused",
		model.StateSyncing:      "Syncing",
		model.StateScanning:     "Checking",
		model.StateNoPeer:       "Disconnected",
		model.StateInSync:       "In sync",
	}
	for s, w := range want {
		if got := label(model.Snapshot{State: s}); got != w {
			t.Errorf("label(%d) = %q, want %q", s, got, w)
		}
	}
	if got := label(model.Snapshot{State: model.StateInSync, Label: "Custom"}); got != "Custom" {
		t.Errorf("snapshot label ignored: %q", got)
	}
}
