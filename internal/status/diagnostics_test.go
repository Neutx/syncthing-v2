package status

import (
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

func errDown() error { return &stclient.Error{Kind: stclient.ErrUnreachable} }

const diagKey = "SyntheticApiKey0123456789abcdefXY" // synthetic; gitleaks:allow

func diagSnapshot() model.Snapshot {
	return model.Snapshot{
		State:    model.StateSyncing,
		Detail:   "peer " + idB + " failed at C:\\Users\\example\\Sync and /home/example/Sync from 192.0.2.44",
		PeerLine: "example-b (Tailscale)",
		Peers: []model.Peer{
			{ID: idA, Name: "private-laptop-name", Addr: "100.64.0.2:22000", Transport: TransportTailscale, Connected: true},
			{ID: idB, Name: "other-private-name", Addr: "192.168.1.20:22000", Transport: TransportLAN, Connected: true},
			{ID: "CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH-IIIIIII-JJJJJJJ", Name: "offline-name"},
		},
		Folders: []model.Folder{
			{ID: "secret-folder-id", Label: "Private Label", Path: `D:\Private\Folder`, State: "syncing", GlobalBytes: 2 << 30, NeedBytes: 1 << 20, NeedItems: 3, GlobalFiles: 1200, LocalFiles: 1199},
			{ID: "other-id", Label: "Other", Path: "/home/example/other", Paused: true},
		},
		Pct: 99, NeedBytes: 1 << 20, NeedItems: 3, InRate: 3 << 20, OutRate: 2048, Moving: true, ETA: "1s",
		SizeLine: "2.0 GB of 2.0 GB in sync",
		Activity: []model.ActivityItem{
			{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Text: "Received secret-plan.docx", Tint: TintBlue},
			{At: time.Date(2026, 1, 2, 3, 4, 4, 0, time.UTC), Text: "Device connected", Tint: TintGreen},
			{At: time.Date(2026, 1, 2, 3, 4, 3, 0, time.UTC), Text: "Could not open: /home/example/private", Tint: TintRed},
		},
		Startup:        model.Startup{Syncthing: true, Tray: false, SyncthingManagedByUs: true},
		GUIURL:         "http://127.0.0.1:18384/",
		Pending:        []model.PendingDevice{{DeviceID: "DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH-IIIIIII-JJJJJJJ-KKKKKKK", Name: "stranger", Addr: netip.MustParseAddrPort("198.51.100.9:22000")}},
		PendingFolders: []model.PendingFolder{{FolderID: "offered-id", Label: "Offered Label", FromDevice: idA}},
		Notices:        []string{"gui-exposed"},
		At:             time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
	}
}

func TestDiagnosticsRedaction(t *testing.T) {
	extra := map[string]string{
		"config":   `<gui><apikey>` + diagKey + `</apikey></gui>`,
		"header":   "X-API-Key: " + diagKey,
		"query":    "apikey=" + diagKey + "&x=1",
		"tailnet":  "peer at 100.100.1.2 and fd7a:115c:a1e0::5",
		"lan":      "gateway 10.1.2.3, v6 fe80::1%eth0 and 2001:db8::7",
		"loopback": "listening on 127.0.0.1:18384 and [::1]:18384",
		"paths":    `log at C:\Users\example\AppData\Local\SyncThingV2\logs and ~/Library/Logs/x and \\server\share`,
		"device":   "full id " + strings.ReplaceAll(idA, "-", ""),
		"api":      "GET /rest/db/status failed",
	}
	out := Diagnostics(diagSnapshot(), extra)

	for _, secret := range []string{
		diagKey, "private-laptop-name", "other-private-name", "offline-name", "stranger", "Private Label",
		"secret-folder-id", "Offered Label", "offered-id", "secret-plan", `D:\Private`, "/home/example", `C:\Users`,
		"192.0.2.44", "192.168.1.20", "198.51.100.9", "10.1.2.3", "fe80::1", "2001:db8::7", "fd7a:115c:a1e0::5",
		"~/Library", `\\server`,
	} {
		if strings.Contains(out, secret) {
			t.Errorf("diagnostics leak %q:\n%s", secret, out)
		}
	}

	// Device IDs appear only as their first 7 characters.
	fullID := regexp.MustCompile(`[A-Z2-7]{7}-[A-Z2-7]{7}|[A-Z2-7]{8,}`)
	if m := fullID.FindString(strings.ReplaceAll(out, "[REDACTED]", "")); m != "" {
		t.Errorf("diagnostics contain a device ID longer than 7 characters (%q):\n%s", m, out)
	}

	for _, want := range []string{
		"Status: Syncing",
		"Devices: 3 configured, 2 connected",
		"  AAAAAAA  connected via Tailscale  100.64.0.2:22000",
		"  BBBBBBB  connected via local network  <ip>",
		"  CCCCCCC  not connected",
		"Folders: 2",
		"  #1  state=syncing paused=false errors=0 need=1.0 MB (3 items) global=2.0 GB files=1,199/1,200",
		"  #2  state=unknown paused=true",
		"Synced: 99%  (2.0 GB of 2.0 GB in sync)",
		"Pending: 3 items, 1.0 MB",
		"Speed: down 3.0 MB/s, up 2.0 KB/s  (ETA 1s)",
		"Startup: Syncthing ON / Tray OFF (Syncthing entry managed by SyncThing V2)",
		"Web UI: http://127.0.0.1:18384/",
		"Pending devices: 1",
		"  DDDDDDD  verified=false  <ip>",
		"Pending folders: 1",
		"Notices: gui-exposed",
		"03:04:05  blue  Received <file>",
		"03:04:04  green Device connected",
		"03:04:03  red   Could not open: <path>",
		"Detail: peer BBBBBBB failed at <path> and <path> from <ip>",
		"tailnet: peer at 100.100.1.2 and <ip>",
		"loopback: listening on 127.0.0.1:18384 and [::1]:18384",
		"device: full id AAAAAAA",
		"api: GET /rest/db/status failed",
		"Generated: 2026-01-02T03:04:06Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostics lack %q:\n%s", want, out)
		}
	}
}

// A registered API key is removed even where no key name precedes it, and
// even though no application log was opened (applog.Default() is nil here).
func TestDiagnosticsRedactsBareRegisteredKey(t *testing.T) {
	const bare = "BareDiagKey-Synthetic-9Kd3Lm7Qp2" // synthetic; gitleaks:allow
	if applog.Default() != nil {
		t.Fatal("test expects no default logger")
	}
	applog.AddSecret(bare)
	s := diagSnapshot()
	s.Detail = "request failed: " + bare
	out := Diagnostics(s, map[string]string{"note": "the key is " + bare + " today"})
	if strings.Contains(out, bare) {
		t.Fatalf("diagnostics contain the bare registered key:\n%s", out)
	}
	for _, want := range []string{"note: the key is [REDACTED] today", "Detail: request failed: [REDACTED]"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostics lack %q:\n%s", want, out)
		}
	}
}

func TestDiagnosticsDownState(t *testing.T) {
	s := compute(view{err: errDown(), host: "127.0.0.1:18384"})
	out := Diagnostics(s, nil)
	for _, want := range []string{"Status: Not running", "Detail: Syncthing is not responding on 127.0.0.1:18384", "Devices: 0 configured, 0 connected", "Speed: idle"} {
		if !strings.Contains(out, want) {
			t.Errorf("down diagnostics lack %q:\n%s", want, out)
		}
	}
}

func TestShort(t *testing.T) {
	if Short(idA) != "AAAAAAA" || Short("ABC") != "ABC" || Short("") != "" {
		t.Error("Short")
	}
}
