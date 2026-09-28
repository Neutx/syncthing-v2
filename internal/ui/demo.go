package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// DemoViews lists the views ?demo=<view> understands. "pair" and "settings"
// also select the dashboard view of the same name.
var DemoViews = []string{
	"status", "syncing", "scanning", "disconnected", "paused", "error",
	"down", "unauthorized", "notices", "pair", "settings",
}

// Demo returns a Backend that serves synthetic data only: no Syncthing, no
// Tailscale, no real names, addresses or files. `stv2 demo` and the
// screenshot script use it.
func Demo() Backend { return DemoWith(nil) }

// DemoWith is Demo with a caller-supplied backdrop PNG (for example
// DemoWallpaper passed through the glass pipeline). With nil, the backdrop
// is the plain DemoWallpaper.
func DemoWith(backdropPNG []byte) Backend {
	d := &demo{start: time.Now(), backdrop: backdropPNG}
	d.startup = model.Startup{Syncthing: true, Tray: true, SyncthingManagedByUs: true}
	d.settings = demoSettings{Notifications: true, CheckForUpdates: true, Profile: "tailnet"}
	return d
}

type demoSettings struct {
	Notifications   bool   `json:"notifications"`
	CheckForUpdates bool   `json:"checkForUpdates"`
	Profile         string `json:"profile"`
}

type demo struct {
	start time.Time

	mu       sync.Mutex
	notes    []model.ActivityItem // from actions, newest first
	paused   bool
	startup  model.Startup
	settings demoSettings

	bdOnce   sync.Once
	backdrop []byte
}

// Synthetic devices. Tailnet addresses are inside 100.64.0.0/10 and the IDs
// only look like Syncthing device IDs.
var demoPeers = []model.Peer{
	{ID: "7QKX2PL-ZT4MWEA-B3NDR6Q-JX5YUK2-CV7HSLM-P4TQGZE-WN2KDXF-R6YBJAQ", Name: "studio-desktop", Addr: "100.88.12.34:22000", Transport: "Tailscale", Connected: true},
	{ID: "M4TRV9C-KD2QXNB-7HWZP3E-LF6JMUA-Y2RCGT5-QX8VNDS-B4KEZWM-J7PHAQD", Name: "travel-laptop", Addr: "100.97.41.8:22000", Transport: "Tailscale", Connected: true},
	{ID: "HB2ZW5N-QE7RTXL-M3KDVAP-6YCJWSF-UZ4GNBE-T2QLXKH-R5MVPDC-A7WNJYE", Name: "media-server", Addr: "192.168.50.20:22000", Transport: "local network", Connected: true},
}

type demoFolder struct {
	id, label, path     string
	global, files, need int64
}

var demoFolders = []demoFolder{
	{"docs-2k7qm", "Documents", "~/Sync/Documents", 2_254_857_830, 4_812, 0},
	{"phot-8x3vd", "Photos", "~/Sync/Photos", 41_446_000_000, 18_305, 0},
	{"proj-5w9ta", "Projects", "~/Sync/Projects", 5_690_000_000, 23_977, 0},
}

// demoBase is the Recent Activity history, newest first.
var demoBase = []struct{ text, tint string }{
	{"Up to date", "green"},
	{"Received Documents/Budget 2026.xlsx", "blue"},
	{"Remote change: Photos/2026-08-14 Lake.jpg", "blue"},
	{"Device connected", "green"},
	{"Local change: Notes/ideas.md", "blue"},
	{"Sync started", "blue"},
	{"Checking for changes", "amber"},
	{"Updated Projects/site/index.html", "blue"},
	{"Deleted Projects/old-draft.txt", "blue"},
	{"Local changes: 12 items", "blue"},
	{"Settings updated", "grey"},
	{"Received Photos/2026-08-12 Harbour.jpg", "blue"},
	{"Device disconnected", "red"},
	{"Up to date", "green"},
	{"Received Documents/Travel checklist.pdf", "blue"},
	{"Remote change: Projects/app/main.go", "blue"},
	{"Sync started", "blue"},
	{"Checking for changes", "amber"},
	{"Updated Documents/Reading list.md", "blue"},
	{"Device connected", "green"},
}

func (d *demo) Backdrop() []byte {
	d.bdOnce.Do(func() {
		if d.backdrop != nil {
			return
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, DemoWallpaper(460, 640)); err == nil {
			d.backdrop = buf.Bytes()
		}
	})
	return d.backdrop
}

// DemoWallpaper renders a neutral synthetic "desktop": a dark diagonal blend
// with three soft colour fields. It stands in for a screen capture.
func DemoWallpaper(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	type blob struct{ x, y, r, cr, cg, cb float64 }
	blobs := []blob{
		{0.18, 0.12, 0.55, 64, 120, 255},
		{0.92, 0.46, 0.50, 40, 200, 170},
		{0.30, 0.95, 0.60, 170, 80, 230},
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			u, v := float64(x)/float64(w), float64(y)/float64(h)
			t := (u + v) / 2
			r, g, b := 14+10*t, 18+8*t, 34+20*(1-t)
			for _, bl := range blobs {
				dx, dy := u-bl.x, (v-bl.y)*float64(h)/float64(w)
				k := math.Exp(-(dx*dx + dy*dy) / (bl.r * bl.r * 0.35))
				r += bl.cr * 0.55 * k
				g += bl.cg * 0.55 * k
				b += bl.cb * 0.55 * k
			}
			img.SetRGBA(x, y, color.RGBA{clamp8(r), clamp8(g), clamp8(b), 255})
		}
	}
	return img
}

func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func (d *demo) Snapshots() (<-chan model.Snapshot, func()) { return d.SnapshotsFor("status") }

// SnapshotsFor streams a live synthetic snapshot for view once a second.
func (d *demo) SnapshotsFor(view string) (<-chan model.Snapshot, func()) {
	known := false
	for _, v := range DemoViews {
		known = known || v == view
	}
	if !known {
		view = "status"
	}
	ch := make(chan model.Snapshot, 1)
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case ch <- d.snapshot(view, time.Now()):
			case <-stop:
				return
			}
			select {
			case <-t.C:
			case <-stop:
				return
			}
		}
	}()
	return ch, func() { once.Do(func() { close(stop) }) }
}

func (d *demo) snapshot(view string, now time.Time) model.Snapshot {
	d.mu.Lock()
	paused, startup := d.paused, d.startup
	notes := append([]model.ActivityItem(nil), d.notes...)
	d.mu.Unlock()

	el := now.Sub(d.start).Seconds()
	s := model.Snapshot{
		At:      now,
		Startup: startup,
		GUIURL:  "http://127.0.0.1:8384/",
	}
	s.Peers = append([]model.Peer(nil), demoPeers...)
	var global, localFiles, globalFiles int64
	for _, f := range demoFolders {
		s.Folders = append(s.Folders, model.Folder{
			ID: f.id, Label: f.label, Path: f.path, State: "idle",
			GlobalBytes: f.global, InSyncBytes: f.global, GlobalFiles: f.files, LocalFiles: f.files,
		})
		global += f.global
		globalFiles += f.files
		localFiles += f.files
	}

	state := model.StateInSync
	switch view {
	case "syncing":
		state = model.StateSyncing
	case "scanning":
		state = model.StateScanning
	case "disconnected":
		state = model.StateNoPeer
	case "paused":
		state = model.StatePaused
	case "error":
		state = model.StateError
	case "down":
		state = model.StateDown
	case "unauthorized":
		state = model.StateUnauthorized
	}
	if paused && (state == model.StateInSync || state == model.StateSyncing || state == model.StateScanning) {
		state = model.StatePaused
	}

	var need, needItems int64
	pct := 100
	switch state {
	case model.StateSyncing:
		// Progress climbs from 38 % to 99 % and starts over, like a long transfer.
		pct = 38 + int(el/1.5)%62
		s.InRate = 3.4 * 1024 * 1024 * (1 + 0.25*math.Sin(el/3))
		s.OutRate = 380 * 1024 * (1 + 0.4*math.Sin(el/5))
	case model.StateNoPeer:
		pct = 83
		for i := range s.Peers {
			s.Peers[i].Connected = false
		}
	case model.StateDown, model.StateUnauthorized:
		pct = 0
		s.Peers = nil
		s.Folders = nil
	}
	if state == model.StateSyncing || state == model.StateNoPeer {
		need = global * int64(100-pct) / 100
		needItems = int64(100-pct) * 37
		ph := &s.Folders[1] // Photos carries the outstanding data
		ph.NeedBytes, ph.NeedItems = need, needItems
		ph.InSyncBytes = ph.GlobalBytes - need
		ph.LocalFiles = ph.GlobalFiles - needItems
		ph.State = "syncing"
		if state == model.StateNoPeer {
			ph.State = "idle"
		}
		localFiles -= needItems
	}
	if state == model.StateScanning {
		s.Folders[0].State = "scanning"
	}
	if state == model.StateError {
		s.Folders[2].Errors = 2
	}
	if state == model.StatePaused {
		for i := range s.Folders {
			s.Folders[i].Paused = true
		}
	}
	s.State, s.Pct, s.NeedBytes, s.NeedItems = state, pct, need, needItems
	s.Moving = s.InRate > 1024 || s.OutRate > 1024
	if need > 0 && s.InRate > 1024 {
		s.ETA = demoETA(float64(need) / s.InRate)
	}

	var up []string
	for _, p := range s.Peers {
		if p.Connected {
			up = append(up, p.Name+" ("+string(p.Transport)+")")
		}
	}
	s.PeerLine = "No devices connected"
	if len(up) > 0 {
		s.PeerLine = strings.Join(up, ", ")
	}
	if s.Moving {
		s.SpeedLine = "v " + demoRate(s.InRate) + "   ^ " + demoRate(s.OutRate)
	} else {
		s.SpeedLine = "idle"
	}
	if len(s.Folders) > 0 {
		s.FilesLine = demoGrp(localFiles) + " of " + demoGrp(globalFiles) + " files"
		s.SizeLine = demoSize(global-need) + " of " + demoSize(global) + " in sync"
	}
	s.Label, s.Headline, s.Subline, s.FolderLine = demoText(s)

	switch state {
	case model.StateDown:
		s.Detail = "Syncthing is not responding on 127.0.0.1:8384"
	case model.StateUnauthorized:
		s.Detail = "Syncthing rejected the API key — run `stv2 doctor`"
	case model.StateError:
		s.Detail = "Projects: 2 items could not be synced"
	}

	s.Activity = demoActivity(d.start, el, view, notes)

	switch view {
	case "pair":
		s.Pending = []model.PendingDevice{{
			DeviceID: "Q2WMRTY-HX7KLPD-3ZCVBNE-FA5GUJW-K8SQDRT-N2YXPLM-C6VBHZQ-T4JWEKA",
			Name:     "family-pc",
			Addr:     netip.MustParseAddrPort("100.101.7.22:22000"),
			Verified: true,
			Node: &model.Candidate{
				NodeName: "family-pc", DNSName: "family-pc.example-tailnet.ts.net", OS: "windows",
				LoginName: "sam@example.com", IP: netip.MustParseAddr("100.101.7.22"), SameOwner: false,
				DeviceID: "Q2WMRTY-HX7KLPD-3ZCVBNE-FA5GUJW-K8SQDRT-N2YXPLM-C6VBHZQ-T4JWEKA", Status: "ready",
			},
		}}
		s.PendingFolders = []model.PendingFolder{{FolderID: "rcp-4n8wz", Label: "Recipes", FromDevice: demoPeers[1].ID}}
	case "notices":
		s.Notices = []string{"gui-exposed", "legacy-tray", "tailscale-missing"}
		s.UpdateAvailable = "v1.1.0"
	}
	return s
}

func demoText(s model.Snapshot) (label, headline, subline, folderLine string) {
	switch s.State {
	case model.StateSyncing:
		sub := strconv.Itoa(s.Pct) + "% complete"
		if s.ETA != "" {
			sub += "  -  about " + s.ETA + " left"
		}
		return "Syncing", "Syncing", sub, strconv.Itoa(s.Pct) + "% - " + demoSize(s.NeedBytes) + " left"
	case model.StateScanning:
		return "Checking", "Checking for changes", "Routine verification - nothing is wrong.", "Checking for changes"
	case model.StateNoPeer:
		return "Disconnected", "Disconnected", "No paired device is reachable right now.",
			strconv.Itoa(s.Pct) + "% - " + demoSize(s.NeedBytes) + " left"
	case model.StateError:
		return "Error", "Needs attention", "2 item(s) could not be synced.", "2 error(s)"
	case model.StatePaused:
		return "Paused", "Paused", "Syncing is paused.", "Paused"
	case model.StateDown:
		return "Not running", "Syncthing not running", "Start Syncthing to resume syncing.", "Up to date"
	case model.StateUnauthorized:
		return "API key rejected", "API key rejected", "Syncthing refused the connection. Run stv2 doctor.", "Up to date"
	}
	return "In sync", "In sync", "All devices hold the same files.", "Up to date"
}

// demoActivity builds 40 items, newest first: action notes, then (while
// syncing) a new "Received" line every 6 s, then the fixed history.
func demoActivity(start time.Time, el float64, view string, notes []model.ActivityItem) []model.ActivityItem {
	out := append([]model.ActivityItem(nil), notes...)
	if view == "syncing" {
		for i := int(el / 6); i >= 1 && len(out) < 40; i-- {
			out = append(out, model.ActivityItem{
				At:   start.Add(time.Duration(i*6) * time.Second),
				Text: fmt.Sprintf("Received Photos/2026-09 Trip/IMG_%04d.jpg", 4200+i),
				Tint: "blue",
			})
		}
	}
	for k := 0; len(out) < 40; k++ {
		b := demoBase[k%len(demoBase)]
		out = append(out, model.ActivityItem{
			At:   start.Add(-time.Duration(k*97+11) * time.Second),
			Text: b.text,
			Tint: b.tint,
		})
	}
	return out
}

func (d *demo) note(text, tint string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notes = append([]model.ActivityItem{{At: time.Now(), Text: text, Tint: tint}}, d.notes...)
	if len(d.notes) > 40 {
		d.notes = d.notes[:40]
	}
}

// Action simulates every dashboard command against the synthetic state.
func (d *demo) Action(ctx context.Context, name string, args json.RawMessage) (any, error) {
	var a struct {
		ID, Target, DeviceID, FolderID, FromDevice, Path, Key, Profile, Page string
		On, Yes, Forever                                                     bool
		DeviceIDs                                                            []string
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("invalid args for %s: %w", name, err)
		}
	}
	switch name {
	case "show", "quit", "hide", "open-webui", "open-logs", "open-update",
		"install-tailscale", "fix-gui-exposure", "dismiss-notice", "migrate-legacy":
		return nil, nil
	case "rescan":
		d.note("Rescan requested", "amber")
	case "restart":
		d.note("Restarting Syncthing", "amber")
	case "start-syncthing":
		d.note("Starting Syncthing", "amber")
	case "pause", "resume":
		d.mu.Lock()
		d.paused = name == "pause"
		d.mu.Unlock()
		if name == "pause" {
			d.note("Syncing paused", "grey")
		} else {
			d.note("Syncing resumed", "green")
		}
	case "open-folder":
		for _, f := range demoFolders {
			if a.ID == "" || a.ID == f.id {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("no folder %q", a.ID)
	case "open-docs":
		if _, ok := DocsURL(a.Page); !ok {
			return nil, fmt.Errorf("no documentation page %q", a.Page)
		}
		return nil, nil
	case "allow-firewall":
		// The demo never touches the firewall.
		return map[string]bool{"already": true}, nil
	case "copy-diagnostics":
		return map[string]string{"text": "SyncThing V2 demo diagnostics\nStatus: In sync\nDevices: studio-desktop (Tailscale), travel-laptop (Tailscale), media-server (local network)\nFolders: 3\nStartup: Syncthing ON / Tray ON"}, nil
	case "set-autostart":
		d.mu.Lock()
		defer d.mu.Unlock()
		switch a.Target {
		case "tray":
			d.startup.Tray = a.On
		case "syncthing":
			d.startup.Syncthing = a.On
		default:
			return nil, fmt.Errorf("unknown autostart target %q", a.Target)
		}
	case "discover":
		select {
		case <-time.After(600 * time.Millisecond): // a probe round
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return demoCandidates(), nil
	case "pair":
		if a.DeviceID == "" {
			return nil, fmt.Errorf("pair: missing deviceID")
		}
		d.note("Pairing requested", "blue")
	case "accept-device", "decline-device":
		if a.DeviceID == "" {
			return nil, fmt.Errorf("%s: missing deviceID", name)
		}
	case "share-folder":
		if len(a.DeviceIDs) == 0 {
			return nil, fmt.Errorf("choose at least one device")
		}
		if a.FolderID == "" && a.Path == "" {
			return nil, fmt.Errorf("choose a folder")
		}
		d.note("Folder shared", "green")
	case "pick-folder":
		return map[string]string{"path": "~/Sync/Recipes"}, nil
	case "accept-folder", "decline-folder":
		if a.FolderID == "" || a.FromDevice == "" {
			return nil, fmt.Errorf("%s: missing folder", name)
		}
	case "get-settings":
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.settings, nil
	case "set-pref":
		d.mu.Lock()
		defer d.mu.Unlock()
		switch a.Key {
		case "notifications":
			d.settings.Notifications = a.On
		case "checkForUpdates":
			d.settings.CheckForUpdates = a.On
		default:
			return nil, fmt.Errorf("unknown preference %q", a.Key)
		}
		return d.settings, nil
	case "set-profile":
		if a.Profile != "tailnet" && a.Profile != "hybrid" {
			return nil, fmt.Errorf("unknown profile %q", a.Profile)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.settings.Profile = a.Profile
		return d.settings, nil
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return nil, nil
}

func demoCandidates() []model.Candidate {
	c := func(name, os, login string, ip string, same bool, id, status string) model.Candidate {
		return model.Candidate{
			NodeName: name, DNSName: name + ".example-tailnet.ts.net", OS: os, LoginName: login,
			IP: netip.MustParseAddr(ip), SameOwner: same, DeviceID: id, Status: status,
		}
	}
	return []model.Candidate{
		c("this-computer", "windows", "you@example.com", "100.70.1.2", true, "YH3KD7Q-JW2PLXA-Z6RMVTE-C4NUGBF-Q7XSKDW-L2TMHVA-E5PZRJC-N8BYQWK", "self"),
		c("studio-desktop", "windows", "you@example.com", "100.88.12.34", true, demoPeers[0].ID, "paired"),
		c("travel-laptop", "macOS", "you@example.com", "100.97.41.8", true, demoPeers[1].ID, "paired"),
		c("workshop-mini", "macOS", "you@example.com", "100.66.20.9", true, "D6WQPLE-3KXTRZN-M7HCVBA-J2YFUGS-P5NDKXQ-W8LTAEM-R4ZJCVH-B3QYSNT", "ready"),
		c("old-netbook", "linux", "you@example.com", "100.79.5.60", true, "", "no-syncthing"),
		c("family-pc", "windows", "sam@example.com", "100.101.7.22", false, "Q2WMRTY-HX7KLPD-3ZCVBNE-FA5GUJW-K8SQDRT-N2YXPLM-C6VBHZQ-T4JWEKA", "ready"),
		c("alex-macbook", "macOS", "alex@example.org", "100.90.33.4", false, "", "blocked"),
	}
}

func demoETA(sec float64) string {
	switch {
	case sec < 60:
		return strconv.Itoa(int(sec)) + "s"
	case sec < 3600:
		return strconv.Itoa(int(sec/60)) + "m"
	}
	return strconv.FormatFloat(sec/3600, 'f', 1, 64) + "h"
}

func demoRate(bps float64) string {
	if bps < 1024 {
		return "0 KB/s"
	}
	u := []string{"B", "KB", "MB", "GB"}
	v, i := bps, 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " " + u[i] + "/s"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " " + u[i] + "/s"
}

func demoSize(b int64) string {
	u := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if v < 10 && i > 0 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " " + u[i]
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " " + u[i]
}

func demoGrp(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
