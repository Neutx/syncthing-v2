package app

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/picker"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
	"github.com/Neutx/syncthing-v2/internal/tray"
	"github.com/Neutx/syncthing-v2/internal/uihost"
)

// ---------- a fake Syncthing REST API (synthetic data only) ----------

const testAPIKey = "synthetic-api-key-0123456789" // gitleaks:allow (synthetic test value)

type fakeST struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	myID     string
	devices  map[string]map[string]any
	folders  map[string]map[string]any
	conns    map[string]map[string]any
	pendDevs map[string]map[string]any
	pendFold map[string]any
	options  map[string]any
	writes   []write
}

type write struct {
	Method, Path string
	Body         map[string]any
}

func devID(seed string) string { return deviceid.FromCert([]byte("synthetic certificate " + seed)) }

func newFakeST(t *testing.T) *fakeST {
	f := &fakeST{
		t:        t,
		myID:     devID("self"),
		devices:  map[string]map[string]any{},
		folders:  map[string]map[string]any{},
		conns:    map[string]map[string]any{},
		pendDevs: map[string]map[string]any{},
		pendFold: map[string]any{},
		options: map[string]any{
			"globalAnnounceEnabled": false, "relaysEnabled": false, "natEnabled": false,
			"localAnnounceEnabled": true, "listenAddresses": []any{"default"},
		},
	}
	f.devices[f.myID] = map[string]any{"deviceID": f.myID, "name": "this-computer", "addresses": []any{"dynamic"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeST) endpoint() stclient.Endpoint {
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	return stclient.Endpoint{BaseURL: u, APIKey: testAPIKey}
}

func (f *fakeST) addFolder(id, label, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders[id] = map[string]any{"id": id, "label": label, "path": path, "paused": false, "type": "sendreceive",
		"devices": []any{map[string]any{"deviceID": f.myID}}}
}

func (f *fakeST) addDevice(id, name string, addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := make([]any, len(addrs))
	for i, s := range addrs {
		a[i] = s
	}
	f.devices[id] = map[string]any{"deviceID": id, "name": name, "addresses": a}
}

func (f *fakeST) connect(id, addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[id] = map[string]any{"connected": true, "address": addr, "type": "tcp-client"}
}

func (f *fakeST) writesTo(method, prefix string) []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []write
	for _, w := range f.writes {
		if w.Method == method && strings.HasPrefix(w.Path, prefix) {
			out = append(out, w)
		}
	}
	return out
}

func (f *fakeST) folder(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.folders[id]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeST) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != testAPIKey {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p := r.URL.Path
	if r.Method != http.MethodGet {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.writes = append(f.writes, write{r.Method, r.URL.RequestURI(), body})
		f.mu.Unlock()
		f.apply(w, r, p, body)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case p == "/rest/events":
		// Hold the long-poll briefly, like Syncthing with nothing to report.
		f.mu.Unlock()
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
		}
		f.mu.Lock()
		writeJSON(w, []any{})
	case p == "/rest/system/status":
		writeJSON(w, map[string]any{"myID": f.myID})
	case p == "/rest/config/devices":
		out := []any{}
		for _, id := range sortedIDs(f.devices) {
			out = append(out, f.devices[id])
		}
		writeJSON(w, out)
	case strings.HasPrefix(p, "/rest/config/devices/"):
		if d, ok := f.devices[strings.TrimPrefix(p, "/rest/config/devices/")]; ok {
			writeJSON(w, d)
		} else {
			http.Error(w, "no such device", http.StatusNotFound)
		}
	case p == "/rest/config/folders":
		out := []any{}
		for _, id := range sortedIDs(f.folders) {
			out = append(out, f.folders[id])
		}
		writeJSON(w, out)
	case strings.HasPrefix(p, "/rest/config/folders/"):
		if d, ok := f.folders[strings.TrimPrefix(p, "/rest/config/folders/")]; ok {
			writeJSON(w, d)
		} else {
			http.Error(w, "no such folder", http.StatusNotFound)
		}
	case p == "/rest/db/status":
		writeJSON(w, map[string]any{"globalBytes": 1000, "inSyncBytes": 1000, "globalFiles": 10, "localFiles": 10, "state": "idle"})
	case p == "/rest/system/connections":
		c := map[string]any{}
		for k, v := range f.conns {
			c[k] = v
		}
		writeJSON(w, map[string]any{"total": map[string]any{"inBytesTotal": 0, "outBytesTotal": 0}, "connections": c})
	case p == "/rest/cluster/pending/devices":
		writeJSON(w, f.pendDevs)
	case p == "/rest/cluster/pending/folders":
		writeJSON(w, f.pendFold)
	case p == "/rest/config/options":
		writeJSON(w, f.options)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeST) apply(w http.ResponseWriter, r *http.Request, p string, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(p, "/rest/config/folders/"):
		id := strings.TrimPrefix(p, "/rest/config/folders/")
		switch r.Method {
		case http.MethodPut:
			f.folders[id] = body
		case http.MethodPatch:
			if f.folders[id] == nil {
				http.NotFound(w, r)
				return
			}
			for k, v := range body {
				f.folders[id][k] = v
			}
		}
	case strings.HasPrefix(p, "/rest/config/devices/"):
		id := strings.TrimPrefix(p, "/rest/config/devices/")
		switch r.Method {
		case http.MethodPut:
			f.devices[id] = body
			delete(f.pendDevs, id)
		case http.MethodPatch:
			if f.devices[id] == nil {
				http.NotFound(w, r)
				return
			}
			for k, v := range body {
				f.devices[id][k] = v
			}
		}
	case p == "/rest/cluster/pending/devices" && r.Method == http.MethodDelete:
		delete(f.pendDevs, r.URL.Query().Get("device"))
	case p == "/rest/config/options" && r.Method == http.MethodPatch:
		for k, v := range body {
			f.options[k] = v
		}
	}
	w.WriteHeader(http.StatusOK)
}

func sortedIDs[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ---------- fakes for the platform ----------

type fakeTray struct {
	mu      sync.Mutex
	icons   []model.State
	tooltip string
	menu    []tray.MenuItem
	quit    bool
}

func (t *fakeTray) Run(onReady func(), onClick func(), onMenu func(string)) {}
func (t *fakeTray) SetIcon(s model.State, pct int) {
	t.mu.Lock()
	t.icons = append(t.icons, s)
	t.mu.Unlock()
}
func (t *fakeTray) SetTooltip(s string) { t.mu.Lock(); t.tooltip = s; t.mu.Unlock() }
func (t *fakeTray) SetMenu(m []tray.MenuItem) {
	t.mu.Lock()
	t.menu = m
	t.mu.Unlock()
}
func (t *fakeTray) Balloon(title, body string) bool { return true }
func (t *fakeTray) Quit()                           { t.mu.Lock(); t.quit = true; t.mu.Unlock() }
func (t *fakeTray) state() (string, []tray.MenuItem, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tooltip, t.menu, t.quit
}

type fakeHost struct {
	mu      sync.Mutex
	browser bool
	visible bool
	calls   []string
	views   []string // the view of each show ("" keeps the current one)
	shownAt image.Rectangle
}

func (h *fakeHost) Show(r image.Rectangle) { h.ShowView(r, "") }
func (h *fakeHost) ShowView(r image.Rectangle, view string) {
	h.mu.Lock()
	h.calls = append(h.calls, "show")
	h.views = append(h.views, view)
	h.shownAt, h.visible = r, !h.browser
	h.mu.Unlock()
}
func (h *fakeHost) shownViews() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.views...)
}
func (h *fakeHost) Hide() {
	h.mu.Lock()
	h.calls = append(h.calls, "hide")
	h.visible = false
	h.mu.Unlock()
}
func (h *fakeHost) Visible() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.visible }
func (h *fakeHost) Close() error  { return nil }
func (h *fakeHost) Browser() bool { return h.browser }

var _ uihost.Host = (*fakeHost)(nil)

type fakeAuto struct {
	mu      sync.Mutex
	on      map[autostart.Target]bool
	foreign bool
	loaded  bool // launchd has our job loaded while its entry is on
	sets    []string
}

func (f *fakeAuto) Enabled(t autostart.Target) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on[t], nil
}
func (f *fakeAuto) Existing(t autostart.Target) (string, bool) {
	if t == autostart.Syncthing && f.foreign {
		return "Startup folder shortcut", true
	}
	return "", false
}
func (f *fakeAuto) Loaded(t autostart.Target) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded && f.on[t]
}
func (f *fakeAuto) Set(t autostart.Target, on bool, bin string, args []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on[t] = on
	f.sets = append(f.sets, t.String()+"="+map[bool]string{true: "on", false: "off"}[on]+" "+bin)
	return nil
}

type fakeTS struct{ st tailnet.Status }

func (f fakeTS) Status(context.Context) (tailnet.Status, error) { return f.st, nil }

type notice struct{ title, body string }

type harness struct {
	a       *App
	st      *fakeST
	tray    *fakeTray
	host    *fakeHost
	auto    *fakeAuto
	mu      sync.Mutex
	notes   []notice
	clicks  []func() // the click handler of each notification
	opened  []string
	folders []string
}

func (h *harness) notifications() []notice {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]notice(nil), h.notes...)
}

// newHarness builds an App wired to fakes only: nothing touches the real
// autostart entries, Syncthing, Tailscale or the desktop.
func newHarness(t *testing.T, st *fakeST, gui stclient.GUIConfig) *harness {
	t.Helper()
	p, err := prefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{st: st, tray: &fakeTray{}, host: &fakeHost{}, auto: &fakeAuto{on: map[autostart.Target]bool{}}}
	a := newApp(Options{Background: true}, p, t.TempDir())
	a.tray = h.tray
	a.host = h.host
	a.auto = h.auto
	a.notifyFn = func(title, body string, onClick func()) {
		h.mu.Lock()
		h.notes = append(h.notes, notice{title, body})
		h.clicks = append(h.clicks, onClick)
		h.mu.Unlock()
	}
	a.openURL = func(u string) error { h.mu.Lock(); h.opened = append(h.opened, u); h.mu.Unlock(); return nil }
	a.openFolder = func(p string) error { h.mu.Lock(); h.folders = append(h.folders, p); h.mu.Unlock(); return nil }
	a.pickFolder = func(string, string) (string, error) { return "", nil }
	a.ts = fakeTS{}
	a.locateTS = func() (string, error) { return "/synthetic/tailscale", nil }
	a.roots = func() (install.Roots, error) { return install.Roots{}, errors.New("no install roots in tests") }
	a.hasSNI = func() bool { return true }
	a.startST = func(string) error { return errors.New("starting Syncthing is not allowed in tests") }
	a.allowFW = func(context.Context, string) error {
		return errors.New("changing the firewall is not allowed in tests")
	}
	a.hasFW = func(context.Context) (bool, error) {
		return false, errors.New("reading the firewall is not allowed in tests")
	}
	a.detect = func(context.Context) (stinstall.Install, error) {
		return stinstall.Install{}, stinstall.ErrNotFound
	}
	if st != nil {
		a.discover = func(context.Context, string) (stclient.Endpoint, string, error) {
			return st.endpoint(), "synthetic-config.xml", nil
		}
		a.readConfig = func(string) (stclient.Endpoint, stclient.GUIConfig, error) {
			return st.endpoint(), gui, nil
		}
	} else {
		a.discover = func(context.Context, string) (stclient.Endpoint, string, error) {
			return stclient.Endpoint{}, "", stclient.ErrConfigNotFound
		}
	}
	a.place = func(image.Point) (image.Rectangle, float64) { return image.Rect(100, 50, 560, 690), 1 }
	a.capture = func(r image.Rectangle) (*image.RGBA, error) {
		img := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
		for i := range img.Pix {
			img.Pix[i] = 0x80
		}
		return img, nil
	}
	a.render = func(src *image.RGBA, _ float64) *image.RGBA {
		out := image.NewRGBA(src.Bounds())
		out.Set(0, 0, color.RGBA{1, 2, 3, 255})
		return out
	}
	h.a = a
	t.Cleanup(func() { a.shutdown() })
	return h
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) waitState(t *testing.T, s model.State) model.Snapshot {
	t.Helper()
	var snap model.Snapshot
	waitFor(t, "state "+stateName(s), func() bool {
		h.a.mu.Lock()
		defer h.a.mu.Unlock()
		snap = h.a.last
		return h.a.haveLast && h.a.sess != nil && h.a.haveBase && snap.State == s
	})
	return snap
}

func stateName(s model.State) string {
	return [...]string{"down", "unauthorized", "error", "paused", "syncing", "scanning", "nopeer", "insync"}[s]
}

func act(t *testing.T, a *App, name string, args any) (any, error) {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.Action(ctx, name, raw)
}

// ---------- tests ----------

func TestSnapshotsReachTrayAndSubscribers(t *testing.T) {
	st := newFakeST(t)
	peer := devID("peer")
	st.addDevice(peer, "studio-desktop", "dynamic")
	st.connect(peer, "100.64.0.9:22000")
	st.addFolder("docs-abcde", "Documents", t.TempDir())
	h := newHarness(t, st, stclient.GUIConfig{Address: "127.0.0.1:8384", Port: 8384})

	ch, unsub := h.a.Snapshots()
	h.a.connect(context.Background())
	snap := h.waitState(t, model.StateInSync)

	if snap.PeerLine != "studio-desktop (Tailscale)" {
		t.Errorf("PeerLine = %q", snap.PeerLine)
	}
	tip, menu, _ := h.tray.state()
	if !strings.HasPrefix(tip, "SyncThing V2 - In sync") {
		t.Errorf("tooltip = %q", tip)
	}
	if len(menu) == 0 || menu[0].ID != tray.IDInfoState {
		t.Fatalf("menu not set: %+v", menu)
	}
	// The subscriber receives the latest snapshot.
	waitFor(t, "subscriber snapshot", func() bool {
		select {
		case s := <-ch:
			return s.State == model.StateInSync
		default:
			return false
		}
	})
	unsub()
	h.a.mu.Lock()
	n := len(h.a.subs)
	h.a.mu.Unlock()
	if n != 0 {
		t.Errorf("%d subscribers left after unsubscribe", n)
	}
	// A new subscriber gets the current snapshot first.
	ch2, unsub2 := h.a.Snapshots()
	defer unsub2()
	select {
	case s := <-ch2:
		if s.State != model.StateInSync {
			t.Errorf("first snapshot state %v", s.State)
		}
	default:
		t.Error("new subscriber did not get the latest snapshot first")
	}
}

func TestPauseResumeTogglesEveryFolder(t *testing.T) {
	st := newFakeST(t)
	peer := devID("peer")
	st.addDevice(peer, "studio-desktop", "dynamic")
	st.connect(peer, "100.64.0.9:22000")
	st.addFolder("aaaaa-11111", "One", t.TempDir())
	st.addFolder("bbbbb-22222", "Two", t.TempDir())
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	h.a.connect(context.Background())
	h.waitState(t, model.StateInSync)

	if _, err := act(t, h.a, "pause", nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aaaaa-11111", "bbbbb-22222"} {
		if st.folder(id)["paused"] != true {
			t.Errorf("folder %s not paused", id)
		}
	}
	h.waitState(t, model.StatePaused)
	if _, err := act(t, h.a, "resume", nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aaaaa-11111", "bbbbb-22222"} {
		if st.folder(id)["paused"] != false {
			t.Errorf("folder %s not resumed", id)
		}
	}
	snap := h.waitState(t, model.StateInSync)
	found := false
	for _, it := range snap.Activity {
		found = found || it.Text == "Syncing resumed"
	}
	if !found {
		t.Errorf("no \"Syncing resumed\" note in %+v", snap.Activity)
	}
	// The tray menu offers Pause again once resumed.
	_, menu, _ := h.tray.state()
	if !slices.ContainsFunc(menu, func(m tray.MenuItem) bool { return m.ID == tray.IDPause }) {
		t.Error("menu has no Pause All item")
	}
}

func TestGUIExposureNoticeFixAndDismiss(t *testing.T) {
	st := newFakeST(t)
	h := newHarness(t, st, stclient.GUIConfig{Address: "0.0.0.0:8385", Port: 8385})
	h.a.connect(context.Background())
	snap := h.waitState(t, model.StateInSync)
	if !slices.Contains(snap.Notices, NoticeGUIExposed) {
		t.Fatalf("notices = %v, want gui-exposed", snap.Notices)
	}
	if n := h.notifications(); len(n) != 1 || n[0].title != "Syncthing control panel exposed" {
		t.Errorf("notifications = %+v", n)
	}

	// "Not now" hides it for this launch only.
	if _, err := act(t, h.a, "dismiss-notice", map[string]any{"id": NoticeGUIExposed, "forever": false}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.a.lastSnapshot().Notices, NoticeGUIExposed) {
		t.Error("notice still shown after Not now")
	}
	if h.a.prefs.Get().NoticeDismissed(NoticeGUIExposed) {
		t.Error("Not now was stored as Don't ask again")
	}

	// Restrict sends the exact PATCH of §5.4.
	if _, err := act(t, h.a, "fix-gui-exposure", nil); err != nil {
		t.Fatal(err)
	}
	ws := st.writesTo(http.MethodPatch, "/rest/config/gui")
	if len(ws) != 1 || ws[0].Body["address"] != "127.0.0.1:8385" || ws[0].Body["insecureAdminAccess"] != false {
		t.Fatalf("gui PATCH = %+v", ws)
	}

	// Don't ask again is persisted.
	if _, err := act(t, h.a, "dismiss-notice", map[string]any{"id": NoticeGUIExposed, "forever": true}); err != nil {
		t.Fatal(err)
	}
	if !h.a.prefs.Get().NoticeDismissed(NoticeGUIExposed) {
		t.Error("Don't ask again not stored in prefs")
	}
	if _, err := act(t, h.a, "dismiss-notice", map[string]any{"id": "no-such-notice"}); err == nil {
		t.Error("unknown notice accepted")
	}
}

func TestOnlyVerifiedPairingRequestsAreOffered(t *testing.T) {
	st := newFakeST(t)
	good, lan := devID("tailnet-peer"), devID("lan-peer")
	st.pendDevs[good] = map[string]any{"name": "peer-b", "address": "100.64.0.7:22000"}
	st.pendDevs[lan] = map[string]any{"name": "lan-box", "address": "192.168.1.20:22000"}
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	h.a.ts = fakeTS{st: tailnet.Status{
		BackendState: tailnet.BackendRunning,
		Self:         tailnet.Node{HostName: "this-computer", UserID: 1, IPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")}},
		Peers: []tailnet.Node{{HostName: "peer-b", OS: "linux", Online: true, UserID: 2, LoginName: "sam@example.com",
			IPs: []netip.Addr{netip.MustParseAddr("100.64.0.7")}}},
	}}
	h.a.probe = func(_ context.Context, ap netip.AddrPort) (string, error) {
		if ap.Addr() == netip.MustParseAddr("100.64.0.7") && ap.Port() == 22000 {
			return good, nil
		}
		return "", errors.New("unexpected probe")
	}
	h.a.connect(context.Background())

	var snap model.Snapshot
	waitFor(t, "verified request", func() bool {
		snap = h.a.lastSnapshot()
		return len(snap.Pending) == 1
	})
	pd := snap.Pending[0]
	if pd.DeviceID != good || !pd.Verified {
		t.Fatalf("pending = %+v", pd)
	}
	// The LAN request is never offered (§4.3a).
	for _, p := range h.a.lastSnapshot().Pending {
		if p.DeviceID == lan {
			t.Fatal("unverified LAN request offered")
		}
	}
	waitFor(t, "pairing notification", func() bool {
		for _, n := range h.notifications() {
			if n.title == "Pairing request" && strings.Contains(n.body, "peer-b wants to sync with this computer") &&
				strings.Contains(n.body, "Owned by sam@example.com") && strings.Contains(n.body, "ID "+good[:7]) {
				return true
			}
		}
		return false
	})

	if _, err := act(t, h.a, "accept-device", map[string]any{"deviceID": lan}); err == nil {
		t.Error("accepting an unverified request succeeded")
	}
	if _, err := act(t, h.a, "accept-device", map[string]any{"deviceID": good}); err != nil {
		t.Fatal(err)
	}
	ws := st.writesTo(http.MethodPut, "/rest/config/devices/"+good)
	if len(ws) != 1 {
		t.Fatalf("device PUTs = %+v", ws)
	}
	addrs, _ := ws[0].Body["addresses"].([]any)
	if !slices.Contains(addrs, any("tcp://100.64.0.7:22000")) || ws[0].Body["autoAcceptFolders"] != false {
		t.Errorf("accepted device = %+v", ws[0].Body)
	}
	if len(h.a.lastSnapshot().Pending) != 0 {
		t.Error("request still offered after Accept")
	}
}

func TestWidenOnFirstConnectOnlyForPairedDevices(t *testing.T) {
	st := newFakeST(t)
	ours, manual := devID("paired-by-v2"), devID("manual")
	st.addDevice(ours, "peer-b", "tcp://100.64.0.7:22000", "quic://100.64.0.7:22000")
	st.addDevice(manual, "nas", "tcp://192.168.1.5:22000")
	st.connect(ours, "100.64.0.7:22000")
	st.connect(manual, "192.168.1.5:22000")
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	h.a.connect(context.Background())
	h.waitState(t, model.StateInSync)

	waitFor(t, "widen", func() bool { return h.a.prefs.Get().IsWidened(ours) })
	ws := st.writesTo(http.MethodPatch, "/rest/config/devices/"+ours)
	if len(ws) != 1 {
		t.Fatalf("PATCHes = %+v", ws)
	}
	if addrs, _ := ws[0].Body["addresses"].([]any); !slices.Contains(addrs, any("dynamic")) {
		t.Errorf("widened addresses = %v", ws[0].Body["addresses"])
	}
	time.Sleep(300 * time.Millisecond)
	if ws := st.writesTo(http.MethodPatch, "/rest/config/devices/"+manual); len(ws) != 0 {
		t.Errorf("manually configured device was changed: %+v", ws)
	}
}

func TestPairedByUs(t *testing.T) {
	cases := []struct {
		addrs []string
		want  bool
	}{
		{[]string{"tcp://100.64.0.7:22000", "quic://100.64.0.7:22000"}, true},
		{[]string{"tcp://[fd7a:115c:a1e0::5]:22000"}, true},
		{[]string{"tcp://100.64.0.7:22000", "dynamic"}, false},
		{[]string{"tcp://100.64.0.7:22001"}, false},
		{[]string{"tcp://192.168.1.5:22000"}, false},
		{[]string{"relay://100.64.0.7:22000"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := pairedByUs(c.addrs, tailnet.Prefixes); got != c.want {
			t.Errorf("pairedByUs(%v) = %v, want %v", c.addrs, got, c.want)
		}
	}
}

func TestShareNewFolder(t *testing.T) {
	st := newFakeST(t)
	peer := devID("peer")
	st.addDevice(peer, "peer-b", "tcp://100.64.0.7:22000")
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	h.a.connect(context.Background())
	h.waitState(t, model.StateInSync)

	dir := filepath.Join(t.TempDir(), "Recipes")
	if _, err := act(t, h.a, "share-folder", map[string]any{"path": dir, "deviceIDs": []string{peer}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("folder not created: %v", err)
	}
	ws := st.writesTo(http.MethodPut, "/rest/config/folders/")
	if len(ws) != 1 {
		t.Fatalf("folder PUTs = %+v", ws)
	}
	b := ws[0].Body
	if b["label"] != "Recipes" || b["path"] != dir {
		t.Errorf("folder = %+v", b)
	}
	var ids []string
	for _, d := range b["devices"].([]any) {
		ids = append(ids, d.(map[string]any)["deviceID"].(string))
	}
	if !slices.Contains(ids, peer) || !slices.Contains(ids, st.myID) {
		t.Errorf("folder devices = %v", ids)
	}
	id := strings.TrimPrefix(ws[0].Path, "/rest/config/folders/")
	if len(id) != 11 || id[5] != '-' {
		t.Errorf("folder ID %q is not xxxxx-xxxxx", id)
	}

	for _, bad := range []map[string]any{
		{"path": dir}, // no device
		{"path": "relative/dir", "deviceIDs": []string{peer}}, // not absolute
		{"folderID": "nope0-00000", "deviceIDs": []string{peer}},
		{"path": dir, "deviceIDs": []string{"not-a-device-id"}},
	} {
		if _, err := act(t, h.a, "share-folder", bad); err == nil {
			t.Errorf("share-folder %v succeeded", bad)
		}
	}
}

func TestSettingsAndPrefs(t *testing.T) {
	st := newFakeST(t)
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	h.a.connect(context.Background())
	h.waitState(t, model.StateInSync)

	res, err := act(t, h.a, "get-settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := res.(settings); !s.Notifications || !s.CheckForUpdates || s.Profile != "tailnet" {
		t.Errorf("settings = %+v", s)
	}
	res, err = act(t, h.a, "set-pref", map[string]any{"key": "notifications", "on": false})
	if err != nil {
		t.Fatal(err)
	}
	if res.(settings).Notifications || h.a.prefs.Get().Notifications {
		t.Error("notifications still on")
	}
	h.a.notify("should not show", "")
	for _, n := range h.notifications() {
		if n.title == "should not show" {
			t.Error("notification shown while turned off")
		}
	}
	if _, err := act(t, h.a, "set-pref", map[string]any{"key": "bogus", "on": true}); err == nil {
		t.Error("unknown preference accepted")
	}

	res, err = act(t, h.a, "set-profile", map[string]any{"profile": "hybrid"})
	if err != nil {
		t.Fatal(err)
	}
	if res.(settings).Profile != "hybrid" || h.a.prefs.Get().ProfileChosen != prefs.ProfileHybrid {
		t.Errorf("profile after set-profile: %+v / %q", res, h.a.prefs.Get().ProfileChosen)
	}
	if _, err := act(t, h.a, "set-profile", map[string]any{"profile": "open"}); err == nil {
		t.Error("unknown profile accepted")
	}
}

func TestAutostartToggles(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.a.exe = filepath.Join(t.TempDir(), "stv2-synthetic")
	if err := h.a.setAutostart("tray", true); err != nil {
		t.Fatal(err)
	}
	if !h.a.startupCached().Tray {
		t.Error("tray autostart not reported after enabling")
	}
	// Syncthing's entry needs a Syncthing binary.
	if err := h.a.setAutostart("syncthing", true); err == nil {
		t.Error("Syncthing autostart enabled without a binary")
	}
	h.a.mu.Lock()
	h.a.inst, h.a.instFound = stinstall.Install{Bin: filepath.Join(t.TempDir(), "syncthing")}, true
	h.a.mu.Unlock()
	if err := h.a.setAutostart("syncthing", true); err != nil {
		t.Fatal(err)
	}
	if su := h.a.startupCached(); !su.Syncthing || !su.SyncthingManagedByUs {
		t.Errorf("startup = %+v", su)
	}
	// A foreign entry makes the toggle read-only.
	h.auto.foreign = true
	h.auto.on[autostart.Syncthing] = false
	h.a.stMu.Lock()
	h.a.startupAt = time.Time{}
	h.a.stMu.Unlock()
	if err := h.a.setAutostart("syncthing", false); err == nil || err.Error() != autostart.ExternalText {
		t.Errorf("foreign entry: err = %v", err)
	}
	if su := h.a.startupCached(); !su.Syncthing || su.SyncthingManagedByUs {
		t.Errorf("startup with a foreign entry = %+v", su)
	}
	if err := h.a.setAutostart("printer", true); err == nil {
		t.Error("unknown target accepted")
	}
}

func TestAutostartOffRestartsSyncthing(t *testing.T) {
	saved := autostartOffStopsSyncthing
	t.Cleanup(func() { autostartOffStopsSyncthing = saved })
	for _, stops := range []bool{true, false} {
		autostartOffStopsSyncthing = stops
		h := newHarness(t, nil, stclient.GUIConfig{})
		h.auto.loaded = true
		bin := filepath.Join(t.TempDir(), "syncthing")
		var started []string
		h.a.startST = func(b string) error { started = append(started, b); return nil }
		h.a.mu.Lock()
		h.a.inst, h.a.instFound = stinstall.Install{Bin: bin}, true
		h.a.mu.Unlock()

		// Turning off an entry that is not ours to begin with stops nothing.
		if err := h.a.setAutostart("syncthing", false); err != nil {
			t.Fatal(err)
		}
		if len(started) != 0 {
			t.Errorf("stops=%v: Syncthing restarted although no entry of ours was removed", stops)
		}
		if err := h.a.setAutostart("syncthing", true); err != nil {
			t.Fatal(err)
		}
		if err := h.a.setAutostart("syncthing", false); err != nil {
			t.Fatal(err)
		}
		want := []string(nil)
		if stops {
			want = []string{bin}
		}
		if !slices.Equal(started, want) {
			t.Errorf("stops=%v: started %q, want %q", stops, started, want)
		}
		if h.auto.on[autostart.Syncthing] {
			t.Errorf("stops=%v: entry still on", stops)
		}
	}

	// Our entry was written this session for a Syncthing started detached,
	// so launchd never loaded it: turning it off stops nothing, and starting
	// Syncthing again would run a second instance on the same home.
	autostartOffStopsSyncthing = true
	h := newHarness(t, nil, stclient.GUIConfig{})
	var started []string
	h.a.startST = func(b string) error { started = append(started, b); return nil }
	h.a.mu.Lock()
	h.a.inst, h.a.instFound = stinstall.Install{Bin: filepath.Join(t.TempDir(), "syncthing")}, true
	h.a.mu.Unlock()
	h.auto.on[autostart.Syncthing] = true
	if err := h.a.setAutostart("syncthing", false); err != nil {
		t.Fatal(err)
	}
	if len(started) != 0 || h.auto.on[autostart.Syncthing] {
		t.Errorf("job not loaded: started %q, entry on %v; want no restart and the entry off", started, h.auto.on[autostart.Syncthing])
	}

	// A failed restart is reported.
	h = newHarness(t, nil, stclient.GUIConfig{})
	h.auto.loaded = true
	h.a.mu.Lock()
	h.a.inst, h.a.instFound = stinstall.Install{Bin: filepath.Join(t.TempDir(), "syncthing")}, true
	h.a.mu.Unlock()
	h.auto.on[autostart.Syncthing] = true
	if err := h.a.setAutostart("syncthing", false); err == nil || !strings.Contains(err.Error(), "restarting Syncthing") {
		t.Errorf("failed restart: err = %v", err)
	}
}

// On macOS a Syncthing run from inside Syncthing.app is started at login by
// that app's own login item: the toggle shows it as configured outside
// SyncThing V2 and adds no LaunchAgent, which would start a second one.
func TestSyncthingAppManagesAutostart(t *testing.T) {
	saved := goos
	t.Cleanup(func() { goos = saved })
	goos = "darwin"
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.a.mu.Lock()
	h.a.setInstLocked(stinstall.Install{Bin: "/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing"})
	h.a.mu.Unlock()
	if desc, ok := h.a.autoExisting(autostart.Syncthing); !ok || desc != "Syncthing.app manages Syncthing" {
		t.Errorf("autoExisting = %q, %v", desc, ok)
	}
	if st := h.a.startupCached(); !st.Syncthing || st.SyncthingManagedByUs {
		t.Errorf("startup = %+v, want Syncthing on and not managed by us", st)
	}
	err := h.a.setAutostart("syncthing", true)
	var ue userError
	if !errors.As(err, &ue) || err.Error() != autostart.ExternalText {
		t.Errorf("setAutostart = %v, want the read-only notice", err)
	}
	if len(h.auto.sets) != 0 {
		t.Errorf("autostart entries written: %q", h.auto.sets)
	}

	// A Homebrew or managed Syncthing is not taken for an app's.
	h = newHarness(t, nil, stclient.GUIConfig{})
	h.a.mu.Lock()
	h.a.setInstLocked(stinstall.Install{Bin: "/opt/homebrew/bin/syncthing"})
	h.a.mu.Unlock()
	if desc, ok := h.a.autoExisting(autostart.Syncthing); ok {
		t.Errorf("autoExisting(homebrew) = %q", desc)
	}
	// Other systems never look at app bundles.
	goos = "linux"
	h.a.mu.Lock()
	h.a.setInstLocked(stinstall.Install{Bin: "/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing"})
	h.a.mu.Unlock()
	if desc, ok := h.a.autoExisting(autostart.Syncthing); ok {
		t.Errorf("autoExisting on linux = %q", desc)
	}
}

func TestDownWithoutSyncthing(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.a.connect(context.Background())
	s := h.a.lastSnapshot()
	if s.State != model.StateDown || !strings.Contains(s.Detail, "not found") {
		t.Errorf("snapshot = %v %q", s.State, s.Detail)
	}
	_, menu, _ := h.tray.state()
	start := slices.IndexFunc(menu, func(m tray.MenuItem) bool { return m.ID == tray.IDStartSyncthing })
	if start < 0 || !menu[start].Visible {
		t.Error("Start Syncthing is not offered while Syncthing is missing")
	}
	for _, name := range []string{"rescan", "pause", "discover", "fix-gui-exposure"} {
		if _, err := act(t, h.a, name, nil); !errors.Is(err, errNoSyncthing) {
			t.Errorf("%s without Syncthing: %v", name, err)
		}
	}
	// Notes are shown even without an engine.
	h.a.note("Synthetic note", "grey")
	if a := h.a.lastSnapshot().Activity; len(a) == 0 || a[0].Text != "Synthetic note" {
		t.Errorf("activity = %+v", a)
	}
}

func TestShowSequence(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.host.visible = true
	h.a.Show()
	if got := strings.Join(h.host.calls, ","); got != "hide,show" {
		t.Errorf("host calls = %s, want hide then show", got)
	}
	if h.host.shownAt != image.Rect(100, 50, 560, 690) {
		t.Errorf("shown at %v", h.host.shownAt)
	}
	if b := h.a.Backdrop(); len(b) < 8 || string(b[1:4]) != "PNG" {
		t.Error("no backdrop PNG cached")
	}

	// Browser mode: no capture, the host opens the browser.
	h.host.browser, h.host.calls = true, nil
	h.a.capture = func(image.Rectangle) (*image.RGBA, error) {
		t.Error("captured the screen in browser mode")
		return nil, errors.New("unexpected")
	}
	h.a.Show()
	if got := strings.Join(h.host.calls, ","); got != "show" {
		t.Errorf("browser host calls = %s", got)
	}
}

func TestOpenAndMenuActions(t *testing.T) {
	st := newFakeST(t)
	peer := devID("peer")
	st.addDevice(peer, "studio-desktop", "dynamic")
	st.connect(peer, "100.64.0.9:22000")
	dir := t.TempDir()
	st.addFolder("docs-abcde", "Documents", dir)
	st.addFolder("gone-abcde", "Gone", filepath.Join(dir, "missing"))
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	h.a.connect(context.Background())
	h.waitState(t, model.StateInSync)

	if _, err := act(t, h.a, "open-folder", map[string]any{"id": "docs-abcde"}); err != nil {
		t.Fatal(err)
	}
	if _, err := act(t, h.a, "open-folder", map[string]any{"id": "gone-abcde"}); err == nil {
		t.Error("opening a missing folder succeeded")
	}
	if _, err := act(t, h.a, "open-webui", nil); err != nil {
		t.Fatal(err)
	}
	h.a.handleMenu(tray.FolderPrefix + "docs-abcde")
	h.mu.Lock()
	folders, opened := append([]string(nil), h.folders...), append([]string(nil), h.opened...)
	h.mu.Unlock()
	if len(folders) != 2 || folders[0] != dir || folders[1] != dir {
		t.Errorf("opened folders %v", folders)
	}
	if len(opened) != 1 || opened[0] != st.srv.URL+"/" {
		t.Errorf("opened URLs %v", opened)
	}
	res, err := act(t, h.a, "copy-diagnostics", nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.(map[string]string)["text"]
	if text == "" || strings.Contains(text, testAPIKey) {
		t.Errorf("diagnostics empty or leaking the API key: %q", text)
	}
	if _, err := act(t, h.a, "no-such-action", nil); err == nil {
		t.Error("unknown action accepted")
	}
	h.a.handleMenu(tray.IDExit)
	if _, _, quit := h.tray.state(); !quit {
		t.Error("Exit did not quit the tray")
	}
}

func TestPromptTextSameOwner(t *testing.T) {
	id := devID("x")
	_, body := PromptText(model.PendingDevice{DeviceID: id, Node: &model.Candidate{NodeName: "laptop", OS: "macOS", SameOwner: true}})
	want := "laptop wants to sync with this computer · macOS · ID " + id[:7]
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got, _ := expandHome("~/Sync"); got != filepath.Join(home, "Sync") {
		t.Errorf("expandHome(~/Sync) = %q", got)
	}
	abs := filepath.Join(t.TempDir(), "x")
	if got, _ := expandHome(abs); got != abs {
		t.Errorf("expandHome(%q) = %q", abs, got)
	}
	if _, err := expandHome(""); err == nil {
		t.Error("empty path accepted")
	}
}

// ---------- views, connect serialisation and the firewall rule ----------

// TestNoTrayHostWaits: at login the tray host (StatusNotifierWatcher) often
// appears a moment after the tray starts. The UI002 notice and the browser
// fallback fire only if it is still missing after the wait.
func TestNoTrayHostWaits(t *testing.T) {
	shows := func(h *harness) int { return len(h.host.shownViews()) }

	// The watcher appears during the wait: no notice, no dashboard.
	h := newHarness(t, nil, stclient.GUIConfig{})
	var polls atomic.Int32
	h.a.hasSNI = func() bool { return polls.Add(1) > 3 }
	h.a.sniWait, h.a.sniPoll = 5*time.Second, 5*time.Millisecond
	h.a.opts = Options{Background: true}
	h.a.openOnStart()
	waitFor(t, "the watcher polls", func() bool { return polls.Load() > 3 })
	h.a.Quit()
	h.a.wg.Wait()
	if n := len(h.notifications()); n != 0 {
		t.Errorf("%d notifications although the tray host appeared: %+v", n, h.notifications())
	}
	if n := shows(h); n != 0 {
		t.Errorf("background start opened the dashboard %d times although the tray host appeared", n)
	}

	// The watcher never appears: the notice, then the dashboard.
	h = newHarness(t, nil, stclient.GUIConfig{})
	h.a.hasSNI = func() bool { return false }
	h.a.sniWait, h.a.sniPoll = 60*time.Millisecond, 10*time.Millisecond
	h.a.opts = Options{Background: true}
	start := time.Now()
	h.a.openOnStart()
	if len(h.notifications()) != 0 || shows(h) != 0 {
		t.Fatal("the UI002 fallback fired before waiting for the tray host")
	}
	waitFor(t, "the UI002 fallback", func() bool { return shows(h) == 1 })
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("fallback after %v, want at least the 60 ms wait", d)
	}
	if notes := h.notifications(); len(notes) != 1 || notes[0].title != "No tray icon available" {
		t.Errorf("notifications = %+v, want the UI002 notice", notes)
	}

	// Outside the autostart mode the dashboard opens at once; only the
	// notice waits.
	h = newHarness(t, nil, stclient.GUIConfig{})
	h.a.hasSNI = func() bool { return false }
	h.a.sniWait, h.a.sniPoll = time.Hour, time.Hour
	h.a.opts = Options{}
	h.a.openOnStart()
	waitFor(t, "the dashboard", func() bool { return shows(h) == 1 })
	h.a.Quit()
	h.a.wg.Wait()
	if n := len(h.notifications()); n != 0 {
		t.Errorf("%d notifications after quitting during the wait", n)
	}
}

// TestShowViews: "Pair Devices…" and the pairing and folder-offer
// notifications open the Pair view, --setup opens the welcome view, and
// "Open Status Window" keeps the page on its view.
func TestShowViews(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.a.handleMenu(tray.IDPair)
	h.a.handleMenu(tray.IDOpenStatus)
	if got := strings.Join(h.host.shownViews(), ","); got != ViewPair+"," {
		t.Errorf("menu views = %q, want %q", got, ViewPair+",")
	}

	h.host.mu.Lock()
	h.host.views = nil
	h.host.mu.Unlock()
	h.a.onPairingPrompt(model.PendingDevice{DeviceID: devID("peer"), Name: "studio-desktop"})
	h.a.onFolderOffer(model.PendingFolder{FolderID: "docs-abcde", Label: "Documents", FromDevice: devID("peer")})
	h.mu.Lock()
	clicks := append([]func(){}, h.clicks...)
	h.mu.Unlock()
	if len(clicks) != 2 {
		t.Fatalf("%d notifications, want 2", len(clicks))
	}
	for _, c := range clicks {
		c()
	}
	waitFor(t, "two shows", func() bool { return len(h.host.shownViews()) == 2 })
	for _, v := range h.host.shownViews() {
		if v != ViewPair {
			t.Errorf("notification click opened view %q, want %q", v, ViewPair)
		}
	}

	// --setup (and the first run) opens the welcome view.
	h.host.mu.Lock()
	h.host.views = nil
	h.host.mu.Unlock()
	h.a.opts = Options{Setup: true}
	h.a.openOnStart()
	waitFor(t, "the welcome view", func() bool { return len(h.host.shownViews()) == 1 })
	if v := h.host.shownViews()[0]; v != ViewWelcome {
		t.Errorf("setup opened view %q, want %q", v, ViewWelcome)
	}
	// The autostart mode opens nothing.
	h.a.opts = Options{Background: true}
	h.a.openOnStart()
	time.Sleep(50 * time.Millisecond)
	if n := len(h.host.shownViews()); n != 1 {
		t.Errorf("background start opened the dashboard (%d shows)", n)
	}
	// Without a native message box, "About" opens the Settings view, which
	// carries the About section.
	h.host.mu.Lock()
	h.host.views = nil
	h.host.mu.Unlock()
	h.a.opts = Options{}
	h.a.handleMenu(tray.IDAbout)
	waitFor(t, "the about view", func() bool { return len(h.host.shownViews()) == 1 })
	if v := h.host.shownViews()[0]; v != ViewSettings {
		t.Errorf("About opened view %q, want %q", v, ViewSettings)
	}
	// The views the app opens are names the host accepts.
	for _, v := range []string{ViewPair, ViewWelcome, ViewSettings} {
		if !uihost.ValidView(v) {
			t.Errorf("view %q is not a valid uihost view", v)
		}
	}
}

// TestConnectIsSerialised: connect running at the same time from
// connectLoop and afterStart attaches one session, not one per caller.
func TestConnectIsSerialised(t *testing.T) {
	st := newFakeST(t)
	h := newHarness(t, st, stclient.GUIConfig{Port: 8384})
	var mu sync.Mutex
	engines := 0
	newEngine := h.a.newEngine
	h.a.newEngine = func(c *stclient.Client, startup func() model.Startup) engine {
		mu.Lock()
		engines++
		mu.Unlock()
		return newEngine(c, startup)
	}
	discover := h.a.discover
	h.a.discover = func(ctx context.Context, bin string) (stclient.Endpoint, string, error) {
		time.Sleep(50 * time.Millisecond) // keep every caller in flight together
		return discover(ctx, bin)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.a.connect(context.Background())
		}()
	}
	wg.Wait()
	mu.Lock()
	n := engines
	mu.Unlock()
	if n != 1 {
		t.Errorf("%d sessions attached by concurrent connects, want 1", n)
	}
	h.waitState(t, model.StateInSync)
}

func TestAllowFirewall(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	if _, err := act(t, h.a, "allow-firewall", nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("without Syncthing: %v", err)
	}

	bin := "/synthetic/syncthing"
	h.a.mu.Lock()
	h.a.inst, h.a.instFound = stinstall.Install{Bin: bin}, true
	h.a.mu.Unlock()
	var added []string
	h.a.allowFW = func(_ context.Context, b string) error { added = append(added, b); return nil }

	// The rule is already there: no prompt.
	h.a.hasFW = func(context.Context) (bool, error) { return true, nil }
	res, err := act(t, h.a, "allow-firewall", nil)
	if err != nil || res.(map[string]bool)["already"] != true || len(added) != 0 {
		t.Errorf("existing rule: %v, %v, added %v", res, err, added)
	}

	// Missing (or unreadable) rule: it is added for the Syncthing in use.
	h.a.hasFW = func(context.Context) (bool, error) { return false, errors.New("netsh failed") }
	res, err = act(t, h.a, "allow-firewall", nil)
	if err != nil || res.(map[string]bool)["already"] != false || len(added) != 1 || added[0] != bin {
		t.Errorf("new rule: %v, %v, added %v", res, err, added)
	}
	if a := h.a.lastSnapshot().Activity; len(a) == 0 || !strings.Contains(a[0].Text, "Firewall rule added") {
		t.Errorf("activity = %+v", a)
	}

	// A declined UAC prompt reads as a sentence; other errors pass through.
	h.a.allowFW = func(context.Context, string) error { return install.ErrElevationDeclined }
	if _, err := act(t, h.a, "allow-firewall", nil); err == nil || !strings.Contains(err.Error(), "Administrator approval was not given") {
		t.Errorf("declined prompt: %v", err)
	}
	h.a.allowFW = func(context.Context, string) error { return install.ErrFirewallUnsupported }
	if _, err := act(t, h.a, "allow-firewall", nil); !errors.Is(err, install.ErrFirewallUnsupported) {
		t.Errorf("unsupported OS: %v", err)
	}
}

// Until the first look for Syncthing has finished, the tray keeps its
// neutral "starting" look and offers no Start action, and the dashboard says
// it is connecting instead of "Not running".
func TestTrayWaitsForFirstLook(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.a.publish()
	s := h.a.lastSnapshot()
	if !s.Connecting || s.Headline != "Connecting" || s.Detail != "Looking for Syncthing…" {
		t.Errorf("snapshot before the first look = %+v", s)
	}
	h.tray.mu.Lock()
	icons, menu, tip := len(h.tray.icons), h.tray.menu, h.tray.tooltip
	h.tray.mu.Unlock()
	if icons != 0 || menu != nil || tip != "" {
		t.Errorf("tray updated before the state was known: icons %d, menu %v, tooltip %q", icons, menu, tip)
	}

	h.a.connect(context.Background())
	s = h.a.lastSnapshot()
	if s.Connecting || s.State != model.StateDown || !strings.Contains(s.Detail, "not found") {
		t.Errorf("snapshot after the first look = %+v", s)
	}
	_, menu, _ = h.tray.state()
	if !slices.ContainsFunc(menu, func(m tray.MenuItem) bool { return m.ID == tray.IDStartSyncthing && m.Visible }) {
		t.Error("Start Syncthing is not offered once Syncthing is known to be missing")
	}
}

// A failed tray command is noted in Recent Activity exactly once; Start and
// the login toggles also raise a notification.
func TestMenuFailuresNotedOnce(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	count := func(text string) int {
		n := 0
		for _, a := range h.a.lastSnapshot().Activity {
			if strings.Contains(a.Text, text) {
				n++
			}
		}
		return n
	}

	h.a.handleMenu(tray.FolderPrefix + "missing-folder")
	if n := count("No folder path available"); n != 1 {
		t.Errorf("folder failure noted %d times", n)
	}

	h.a.openURL = func(string) error { return errors.New("synthetic open failure") }
	h.a.handleMenu(tray.IDUpdate)
	if n := count("synthetic open failure"); n != 1 {
		t.Errorf("open failure noted %d times", n)
	}

	h.a.handleMenu(tray.IDOpenWebUI) // Syncthing is not running
	if n := count(string(errNoSyncthing)); n != 1 {
		t.Errorf("Open Web UI without Syncthing noted %d times", n)
	}

	h.a.mu.Lock()
	h.a.inst, h.a.instFound = stinstall.Install{Bin: filepath.Join(t.TempDir(), "syncthing")}, true
	h.a.mu.Unlock()
	h.a.discover = func(context.Context, string) (stclient.Endpoint, string, error) {
		return stclient.Endpoint{}, "", errors.New("synthetic discover failure")
	}
	h.a.handleMenu(tray.IDStartSyncthing)
	if n := count("starting Syncthing is not allowed in tests"); n != 1 {
		t.Errorf("start failure noted %d times", n)
	}

	h.a.exe = ""
	h.a.handleMenu(tray.IDAutostartTray)
	if n := count("executable was not found"); n != 1 {
		t.Errorf("startup toggle failure noted %d times", n)
	}

	var got []string
	for _, n := range h.notifications() {
		got = append(got, n.title+": "+n.body)
	}
	want := []string{
		"Syncthing: Start failed: starting Syncthing is not allowed in tests",
		"SyncThing V2: Startup toggle failed: the SyncThing V2 executable was not found",
	}
	if !slices.Equal(got, want) {
		t.Errorf("notifications = %q, want %q", got, want)
	}
}

// On macOS the login item must not point at the disk image or an App
// Translocation copy, which are gone after an eject or restart.
func TestTrayAutostartRefusesTransientMacPaths(t *testing.T) {
	saved := goos
	t.Cleanup(func() { goos = saved })
	goos = "darwin"
	h := newHarness(t, nil, stclient.GUIConfig{})
	for _, exe := range []string{
		"/Volumes/SyncThing V2/SyncThing V2.app/Contents/MacOS/stv2",
		"/private/var/folders/ab/cd/T/AppTranslocation/0A1B/d/SyncThing V2.app/Contents/MacOS/stv2",
	} {
		h.a.exe = exe
		err := h.a.setAutostart("tray", true)
		var ue userError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "Move SyncThing V2 to Applications first") {
			t.Errorf("%s: err = %v", exe, err)
		}
	}
	if len(h.auto.sets) != 0 {
		t.Errorf("login item written: %q", h.auto.sets)
	}
	h.a.exe = "/Applications/SyncThing V2.app/Contents/MacOS/stv2"
	if err := h.a.setAutostart("tray", true); err != nil {
		t.Fatal(err)
	}
	// Turning it off is always allowed.
	h.a.exe = "/Volumes/SyncThing V2/SyncThing V2.app/Contents/MacOS/stv2"
	if err := h.a.setAutostart("tray", false); err != nil {
		t.Errorf("turning the login item off: %v", err)
	}
}

// Without a native folder picker (Linux without zenity or kdialog) the
// pick-folder action tells the page to ask for a typed path.
func TestPickFolderUnavailable(t *testing.T) {
	h := newHarness(t, nil, stclient.GUIConfig{})
	h.a.pickFolder = func(string, string) (string, error) { return "", picker.ErrUnavailable }
	r, err := act(t, h.a, "pick-folder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := r.(map[string]any); !ok || m["unavailable"] != true || m["path"] != "" {
		t.Fatalf("pick-folder without a picker = %#v, want unavailable", r)
	}
	h.a.pickFolder = func(string, string) (string, error) { return "/synthetic/Sync/Docs", nil }
	r, err = act(t, h.a, "pick-folder", nil)
	if m, ok := r.(map[string]any); err != nil || !ok || m["path"] != "/synthetic/Sync/Docs" || m["unavailable"] != nil {
		t.Fatalf("pick-folder = %#v, %v", r, err)
	}
	h.a.pickFolder = func(string, string) (string, error) { return "", errors.New("picker crashed") }
	if _, err := act(t, h.a, "pick-folder", nil); err == nil {
		t.Fatal("a picker failure was not reported")
	}
}
