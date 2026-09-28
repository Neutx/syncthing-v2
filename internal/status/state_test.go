package status

import (
	"errors"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

const (
	idA = "AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"
	idB = "BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH-IIIIIII"
)

func peer(name string, connected bool, addr string) model.Peer {
	p := model.Peer{ID: idA, Name: name, Connected: connected}
	if connected {
		p.Addr, p.Transport = addr, Classify(addr, "tcp-client")
	}
	return p
}

func folder(state string, global, need int64) model.Folder {
	return model.Folder{ID: "exmpl-" + state, Label: "Example", State: state,
		GlobalBytes: global, NeedBytes: need, InSyncBytes: global - need,
		GlobalFiles: 1500, LocalFiles: 1400, NeedItems: 3}
}

var up = []model.Peer{peer("example-b", true, "100.64.0.2:22000")}

func TestStateMachine(t *testing.T) {
	gib := int64(1 << 30)
	cases := []struct {
		name                string
		v                   view
		state               model.State
		folderLine, subline string
		detail              string
		peerLine            string
	}{
		{
			name:  "unreachable → Down",
			v:     view{err: &stclient.Error{Kind: stclient.ErrUnreachable}, host: "127.0.0.1:18384", folders: []model.Folder{folder("idle", 1, 0)}, peers: up},
			state: model.StateDown, folderLine: "", subline: "Start Syncthing to resume syncing.",
			detail: "Syncthing is not responding on 127.0.0.1:18384", peerLine: "Syncthing not running",
		},
		{
			name:  "401/403 → Unauthorized",
			v:     view{err: &stclient.Error{Kind: stclient.ErrUnauthorized, Status: 403}, host: "127.0.0.1:8384"},
			state: model.StateUnauthorized, subline: "Syncthing is running but did not accept the API key.",
			detail: "Syncthing rejected the API key — run stv2 doctor", peerLine: "API key rejected",
		},
		{
			name:  "bad response → Error",
			v:     view{err: &stclient.Error{Kind: stclient.ErrBadResponse, Status: 500}},
			state: model.StateError, subline: "Unexpected response from Syncthing.",
			detail: "Unexpected response from Syncthing", peerLine: "Unexpected response from Syncthing",
		},
		{
			name:  "foreign error counts as Down",
			v:     view{err: errors.New("boom"), host: "127.0.0.1:8384"},
			state: model.StateDown, subline: "Start Syncthing to resume syncing.",
			detail: "Syncthing is not responding on 127.0.0.1:8384", peerLine: "Syncthing not running",
		},
		{
			name:  "no folders, no remotes → InSync + pair hint",
			v:     view{},
			state: model.StateInSync, folderLine: "No folders configured",
			subline: "No folders configured. Pair a device to start.", peerLine: "No devices connected",
		},
		{
			name:  "no folders with a remote",
			v:     view{peers: []model.Peer{peer("example-b", false, "")}},
			state: model.StateInSync, folderLine: "No folders configured",
			subline: "No folders configured.", peerLine: "No devices connected",
		},
		{
			name: "all paused → Paused (above errors)",
			v: view{peers: up, folders: []model.Folder{
				{ID: "a", Paused: true, Errors: 2, GlobalBytes: 10}, {ID: "b", Paused: true, NeedBytes: 5, GlobalBytes: 10}}},
			state: model.StatePaused, folderLine: "Paused", subline: "Syncing is paused.", peerLine: "example-b (Tailscale)",
		},
		{
			name:  "errors → Error (above NoPeer)",
			v:     view{folders: []model.Folder{{ID: "a", Errors: 3, GlobalBytes: 10}}, peers: []model.Peer{peer("example-b", false, "")}},
			state: model.StateError, folderLine: "3 error(s)", subline: "3 item(s) could not be synced.",
			detail:   "3 item(s) could not be synced in 1 folder(s). Open the Syncthing web UI for the file list.",
			peerLine: "No devices connected",
		},
		{
			name:  "no peer, nothing needed → NoPeer",
			v:     view{folders: []model.Folder{folder("idle", gib, 0)}, peers: []model.Peer{peer("example-b", false, "")}},
			state: model.StateNoPeer, folderLine: "Up to date", subline: "No paired device is reachable right now.",
			peerLine: "No devices connected",
		},
		{
			name:  "Disconnected with need: NoPeer above Syncing",
			v:     view{folders: []model.Folder{folder("syncing", 4*gib, gib)}, peers: []model.Peer{peer("example-b", false, "")}},
			state: model.StateNoPeer, folderLine: "75% - 1.0 GB left", subline: "No paired device is reachable right now.",
			peerLine: "No devices connected",
		},
		{
			name:  "no remotes at all → NoPeer with pair hint",
			v:     view{folders: []model.Folder{folder("idle", 10, 0)}},
			state: model.StateNoPeer, folderLine: "Up to date",
			subline: "No paired device is reachable right now. Pair a device to start.", peerLine: "No devices connected",
		},
		{
			name:  "syncing state → Syncing",
			v:     view{folders: []model.Folder{folder("sync-preparing", 1000, 0)}, peers: up},
			state: model.StateSyncing, folderLine: "100% - 0 B left", subline: "100% complete", peerLine: "example-b (Tailscale)",
		},
		{
			name:  "need bytes while idle → Syncing (above Scanning)",
			v:     view{folders: []model.Folder{folder("scanning", 1000, 1)}, peers: up},
			state: model.StateSyncing, folderLine: "99% - 1 B left", subline: "99% complete", peerLine: "example-b (Tailscale)",
		},
		{
			name:  "syncing with ETA",
			v:     view{folders: []model.Folder{folder("syncing", 10*gib, 2*gib)}, peers: up, inRate: 4 << 20, outRate: 512},
			state: model.StateSyncing, folderLine: "80% - 2.0 GB left", subline: "80% complete  -  about 8m left",
			peerLine: "example-b (Tailscale)",
		},
		{
			name:  "scanning → Scanning",
			v:     view{folders: []model.Folder{folder("scan-waiting", 10, 0)}, peers: up},
			state: model.StateScanning, folderLine: "Checking for changes", subline: "Routine verification - nothing is wrong.",
			peerLine: "example-b (Tailscale)",
		},
		{
			name:  "otherwise → InSync",
			v:     view{folders: []model.Folder{folder("idle", 10, 0), folder("cleaning", 5, 0)}, peers: up},
			state: model.StateInSync, folderLine: "Up to date", subline: "All devices hold the same files.",
			peerLine: "example-b (Tailscale)",
		},
		{
			name:  "paused folder backlog is ignored while others run",
			v:     view{folders: []model.Folder{{ID: "a", Paused: true, State: "paused", NeedBytes: 500, GlobalBytes: 1000}, folder("idle", 10, 0)}, peers: up},
			state: model.StateInSync, folderLine: "Up to date", subline: "All devices hold the same files.",
			peerLine: "example-b (Tailscale)",
		},
	}
	for _, c := range cases {
		s := compute(c.v)
		if s.State != c.state || s.FolderLine != c.folderLine || s.Subline != c.subline || s.Detail != c.detail || s.PeerLine != c.peerLine {
			t.Errorf("%s:\n got  state=%s folder=%q sub=%q detail=%q peers=%q\n want state=%s folder=%q sub=%q detail=%q peers=%q",
				c.name, Label(s.State), s.FolderLine, s.Subline, s.Detail, s.PeerLine,
				Label(c.state), c.folderLine, c.subline, c.detail, c.peerLine)
		}
		if s.Label != Label(s.State) || s.Headline != Headline(s.State) {
			t.Errorf("%s: label %q headline %q", c.name, s.Label, s.Headline)
		}
	}
}

func TestComputeLines(t *testing.T) {
	v := view{
		folders: []model.Folder{
			{ID: "a", State: "syncing", GlobalBytes: 3 << 30, NeedBytes: 1 << 30, InSyncBytes: 2 << 30, GlobalFiles: 12000, LocalFiles: 11500, NeedItems: 40},
			{ID: "b", State: "idle", GlobalBytes: 1 << 30, InSyncBytes: 1 << 30, GlobalFiles: 345, LocalFiles: 345, NeedItems: 2},
		},
		peers: []model.Peer{
			peer("example-b", true, "100.64.0.2:22000"),
			{ID: idB, Name: "example-c", Connected: true, Addr: "192.168.1.9:22000", Transport: Classify("192.168.1.9:22000", "tcp-server")},
			{ID: "CCCCCCC", Name: "example-d"},
		},
		inRate: 2.5 * 1024 * 1024, outRate: 700,
	}
	s := compute(v)
	checks := map[string][2]string{
		"PeerLine":   {s.PeerLine, "example-b (Tailscale), example-c (local network)"},
		"FolderLine": {s.FolderLine, "75% - 1.0 GB left"},
		"FilesLine":  {s.FilesLine, "11,845 of 12,345 files"},
		"SizeLine":   {s.SizeLine, "3.0 GB of 4.0 GB in sync"},
		"SpeedLine":  {s.SpeedLine, "v 2.5 MB/s   ^ 0 KB/s"},
		"ETA":        {s.ETA, "6m"},
		"Subline":    {s.Subline, "75% complete  -  about 6m left"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if s.Pct != 75 || s.NeedBytes != 1<<30 || s.NeedItems != 42 || !s.Moving || len(s.Peers) != 3 || len(s.Folders) != 2 {
		t.Errorf("pct %d need %d items %d moving %v peers %d folders %d", s.Pct, s.NeedBytes, s.NeedItems, s.Moving, len(s.Peers), len(s.Folders))
	}

	idle := compute(view{folders: []model.Folder{folder("idle", 10, 0)}, peers: up, inRate: 1024, outRate: 1024})
	if idle.SpeedLine != "idle" || idle.Moving || idle.ETA != "" {
		t.Errorf("idle: speed %q moving %v eta %q", idle.SpeedLine, idle.Moving, idle.ETA)
	}
	down := compute(view{err: &stclient.Error{Kind: stclient.ErrUnreachable}, host: "127.0.0.1:1"})
	if down.FilesLine != "-" || down.SizeLine != "Service offline" || down.SpeedLine != "" || down.Peers != nil || down.Pct != 100 {
		t.Errorf("down lines: %+v", down)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		global, need int64
		want         int
	}{
		{0, 0, 100},
		{0, 5, 99},
		{100, 0, 100},
		{100, 1, 99},
		{1000, 1, 99},
		{100, 50, 50},
		{3, 1, 66},
		{3, 2, 33},
		{100, 100, 0},
		{100, 150, 0},
	}
	for _, c := range cases {
		if got := Percent(c.global, c.need); got != c.want {
			t.Errorf("Percent(%d, %d) = %d, want %d", c.global, c.need, got, c.want)
		}
	}
}

func TestRateMeter(t *testing.T) {
	var r rateMeter
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	r.update(1000, 500, t0)
	if r.in != 0 || r.out != 0 {
		t.Fatalf("first sample gave rates %v/%v", r.in, r.out)
	}
	r.update(1000+4096, 500+2048, t0.Add(2*time.Second))
	if r.in != 2048 || r.out != 1024 {
		t.Fatalf("rates = %v/%v, want 2048/1024", r.in, r.out)
	}
	// Too short an interval: rates and baseline stay.
	r.update(1000+4096+999999, 500+2048, t0.Add(2*time.Second+400*time.Millisecond))
	if r.in != 2048 || r.out != 1024 || r.prevIn != 1000+4096 {
		t.Fatalf("short interval changed state: %+v", r)
	}
	// Counter reset (Syncthing restarted): the dropped counter never yields a rate.
	r.update(100, 500+2048+3000, t0.Add(5*time.Second))
	if r.in != 0 || r.out != 1000 {
		t.Fatalf("after reset rates = %v/%v, want 0/1000", r.in, r.out)
	}
	r.update(100+6000, 500+2048+3000, t0.Add(8*time.Second))
	if r.in != 2000 || r.out != 0 {
		t.Fatalf("after recovery rates = %v/%v, want 2000/0", r.in, r.out)
	}
	r.reset()
	if r != (rateMeter{}) {
		t.Fatal("reset left state")
	}
}
