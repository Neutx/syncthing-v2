package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/model"
)

var (
	manual       = flag.Bool("manual", false, "run manual UI checks (TestDemoServe)")
	manualPort   = flag.Int("manual.port", 18999, "port for TestDemoServe")
	manualMinute = flag.Int("manual.minutes", 15, "how long TestDemoServe keeps serving")
)

func TestListensOnLoopbackOnly(t *testing.T) {
	s := startServer(t, newFake())
	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Fatalf("URL = %q", s.URL())
	}
	addr := s.ln.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() || addr.IP.To4() == nil || addr.Port != s.Port() {
		t.Fatalf("listener %v", addr)
	}
	if _, err := NewServerPort(newFake(), 70000); err == nil {
		t.Fatal("invalid port accepted")
	}
	if _, err := NewServer(nil); err == nil {
		t.Fatal("nil backend accepted")
	}
}

func TestStaticApp(t *testing.T) {
	s := startServer(t, newFake())
	sess := login(t, s)
	for path, ctype := range map[string]string{
		"/":           "text/html",
		"/index.html": "text/html",
		"/app.css":    "text/css",
		"/tokens.css": "text/css",
		"/app.js":     "text/javascript",
		"/anim.js":    "text/javascript",
	} {
		resp := do(t, s, http.MethodGet, path, reqOpts{cookie: sess})
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: status %d, type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		assertSecurityHeaders(t, resp)
	}
	for _, path := range []string{"/server.go", "/web/app.js", "/..%2fserver.go", "/api/nope", "/index.html/"} {
		if resp := do(t, s, http.MethodGet, path, reqOpts{cookie: sess}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
	if resp := do(t, s, http.MethodPost, "/", reqOpts{cookie: sess, xstv2: "1", body: "{}"}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /: status %d", resp.StatusCode)
	}
}

func TestIndexReferencesEveryAsset(t *testing.T) {
	st, err := loadStatic()
	if err != nil {
		t.Fatal(err)
	}
	index := string(st["/index.html"].data)
	// rel="icon" href="data:," stops the browser from asking for /favicon.ico
	// (a 404 and a console error); the CSP allows it through img-src data:.
	for _, ref := range []string{`href="tokens.css"`, `href="app.css"`, `src="anim.js"`, `src="app.js"`, `<link rel="icon" href="data:,">`} {
		if !strings.Contains(index, ref) {
			t.Errorf("index.html lacks %s", ref)
		}
	}
	// The anim.js port must load before app.js uses window.Anim.
	if strings.Index(index, `src="anim.js"`) > strings.Index(index, `src="app.js"`) {
		t.Error("anim.js must be included before app.js")
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s := startServer(t, newFake())
	sess := login(t, s)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/"}, {http.MethodGet, "/api/info"}, {http.MethodGet, "/missing"},
		{http.MethodGet, "/api/backdrop.png"}, {http.MethodOptions, "/api/action"}, {http.MethodDelete, "/login"},
	} {
		resp := do(t, s, r.method, r.path, reqOpts{cookie: sess, origin: s.URL()})
		assertSecurityHeaders(t, resp)
	}
}

// readEvent reads one SSE event (skipping comments and retry lines).
func readEvent(t *testing.T, br *bufio.Reader) (event, data string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && event != "":
			return event, data
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestStateStream(t *testing.T) {
	f := newFake()
	s := startServer(t, f)
	sess := login(t, s)
	f.snaps <- model.Snapshot{
		State: model.StateSyncing, Pct: 42, Headline: "Syncing",
		Peers:    []model.Peer{{ID: "AAAAAAA-BBBBBBB", Name: "peer-one", Connected: true, Transport: "Tailscale"}},
		Activity: []model.ActivityItem{{At: time.Unix(1700000000, 0), Text: "Sync started", Tint: "blue"}},
	}
	resp := do(t, s, http.MethodGet, "/api/state", reqOpts{cookie: sess})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	assertSecurityHeaders(t, resp)
	br := bufio.NewReader(resp.Body)
	ev, data := readEvent(t, br)
	if ev != "snapshot" {
		t.Fatalf("event %q", ev)
	}
	var got struct {
		StateName string
		Pct       int
		Headline  string
		Peers     []model.Peer
		Activity  []model.ActivityItem
	}
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	if got.StateName != "syncing" || got.Pct != 42 || got.Headline != "Syncing" || len(got.Peers) != 1 ||
		got.Peers[0].Name != "peer-one" || len(got.Activity) != 1 {
		t.Fatalf("snapshot = %+v", got)
	}

	f.snaps <- model.Snapshot{State: model.StateUnauthorized}
	if _, data := readEvent(t, br); !strings.Contains(data, `"StateName":"unauthorized"`) {
		t.Fatalf("second event: %s", data)
	}

	// Closing the server ends the stream and unsubscribes.
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, br)
		done <- err
	}()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after Close")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := f.unsubs
		f.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unsubscribe calls = %d, want 1", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStateNames(t *testing.T) {
	for st := model.StateDown; st <= model.StateInSync; st++ {
		if toWire(model.Snapshot{State: st}).StateName == "" {
			t.Errorf("state %d has no wire name", st)
		}
	}
	if got := toWire(model.Snapshot{State: model.State(99)}).StateName; got != "down" {
		t.Errorf("unknown state maps to %q", got)
	}
}

func postAction(t *testing.T, s *Server, sess, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp := do(t, s, http.MethodPost, "/api/action", reqOpts{cookie: sess, xstv2: "1", origin: s.URL(), body: body})
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestActionDispatch(t *testing.T) {
	f := newFake()
	f.result = map[string]string{"path": "/tmp/x"}
	s := startServer(t, f)
	sess := login(t, s)

	resp, out := postAction(t, s, sess, `{"name":"pick-folder"}`)
	if resp.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("pick-folder: %d %v", resp.StatusCode, out)
	}
	if res, _ := out["result"].(map[string]any); res["path"] != "/tmp/x" {
		t.Fatalf("result = %v", out["result"])
	}
	resp, _ = postAction(t, s, sess, `{"name":"set-autostart","args":{"target":"tray","on":true}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set-autostart: %d", resp.StatusCode)
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last.name != "set-autostart" || last.args != `{"target":"tray","on":true}` {
		t.Fatalf("backend got %+v", last)
	}

	for body, want := range map[string]int{
		`{"name":"quit"}`:                  http.StatusBadRequest, // reserved for the bearer route
		`{"name":"show"}`:                  http.StatusBadRequest,
		`{"name":"format-disk"}`:           http.StatusBadRequest,
		`{"name":"rescan","extra":1}`:      http.StatusBadRequest,
		`not json`:                         http.StatusBadRequest,
		`{"name":"rescan","args":{bad}}`:   http.StatusBadRequest,
		`{"name":"hide","args":null}`:      http.StatusOK,
		`{"name":"open-folder","args":{}}`: http.StatusOK,
	} {
		before := len(f.callNames())
		resp, _ := postAction(t, s, sess, body)
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", body, resp.StatusCode, want)
		}
		dispatched := len(f.callNames()) > before
		if dispatched != (want == http.StatusOK) {
			t.Errorf("%s: dispatched=%v", body, dispatched)
		}
	}

	big := `{"name":"share-folder","args":{"path":"` + strings.Repeat("a", maxActionBody) + `"}}`
	if resp, _ := postAction(t, s, sess, big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}

	f.set(func() { f.err = errors.New("Syncthing is not responding") })
	resp, out = postAction(t, s, sess, `{"name":"restart"}`)
	if resp.StatusCode != http.StatusInternalServerError || out["ok"] != false || out["error"] != "Syncthing is not responding" {
		t.Fatalf("failing action: %d %v", resp.StatusCode, out)
	}

	if resp := do(t, s, http.MethodGet, "/api/action", reqOpts{cookie: sess}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/action: %d", resp.StatusCode)
	}
}

func TestActionsTable(t *testing.T) {
	names := Actions()
	if !sort.StringsAreSorted(names) {
		t.Fatal("Actions() not sorted")
	}
	// Every action in spec §3.9 is accepted.
	for _, n := range []string{
		"rescan", "pause", "resume", "restart", "start-syncthing", "open-folder", "open-webui",
		"copy-diagnostics", "set-autostart", "discover", "pair", "accept-device", "decline-device",
		"share-folder", "pick-folder", "accept-folder", "decline-folder", "fix-gui-exposure",
		"dismiss-notice", "migrate-legacy", "install-tailscale", "hide",
	} {
		if _, ok := actionArgs[n]; !ok {
			t.Errorf("spec action %q missing", n)
		}
	}
	for _, n := range []string{"show", "quit"} {
		if _, ok := actionArgs[n]; ok {
			t.Errorf("%q must only be reachable with the control token", n)
		}
	}
	// Every action app.js sends is in the table.
	st, err := loadStatic()
	if err != nil {
		t.Fatal(err)
	}
	sent := sentActions(string(st["/app.js"].data))
	if len(sent) < 20 {
		t.Fatalf("found only %d actions in app.js; the scanner is out of date", len(sent))
	}
	for n := range sent {
		if _, ok := actionArgs[n]; !ok {
			t.Errorf("app.js sends unknown action %q", n)
		}
	}
}

// sentActions finds act('<name>' calls and runBtn(btn, '<name>' calls in app.js.
func sentActions(js string) map[string]bool {
	out := map[string]bool{}
	for _, prefix := range []string{"act('", "runBtn(btn, '"} {
		rest := js
		for {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i+len(prefix):]
			if j := strings.IndexByte(rest, '\''); j > 0 {
				out[rest[:j]] = true
			}
		}
	}
	return out
}

func TestBackdropAndInfo(t *testing.T) {
	f := newFake()
	s := startServer(t, f)
	sess := login(t, s)
	if resp := do(t, s, http.MethodGet, "/api/backdrop.png", reqOpts{cookie: sess}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no backdrop: %d", resp.StatusCode)
	}
	png := []byte("\x89PNG\r\n\x1a\nfake")
	f.set(func() { f.backdrop = png })
	resp := do(t, s, http.MethodGet, "/api/backdrop.png", reqOpts{cookie: sess})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || string(body) != string(png) {
		t.Fatalf("backdrop: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	resp = do(t, s, http.MethodGet, "/api/info", reqOpts{cookie: sess})
	var in info
	if err := json.NewDecoder(resp.Body).Decode(&in); err != nil {
		t.Fatal(err)
	}
	if in.Disclaimer != brand.Disclaimer || in.Name != brand.DisplayName || in.AtLogin != brand.AtLogin() || in.Version != brand.Version {
		t.Fatalf("info = %+v", in)
	}
}

func TestDemoViews(t *testing.T) {
	d := Demo().(*demo)
	want := map[string]model.State{
		"status": model.StateInSync, "syncing": model.StateSyncing, "scanning": model.StateScanning,
		"disconnected": model.StateNoPeer, "paused": model.StatePaused, "error": model.StateError,
		"down": model.StateDown, "unauthorized": model.StateUnauthorized, "notices": model.StateInSync,
		"pair": model.StateInSync, "settings": model.StateInSync, "no-such-view": model.StateInSync,
	}
	for view, st := range want {
		ch, cancel := d.SnapshotsFor(view)
		var s model.Snapshot
		select {
		case s = <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no snapshot", view)
		}
		cancel()
		if s.State != st {
			t.Errorf("%s: state %d, want %d", view, s.State, st)
		}
		if s.Headline == "" || s.Subline == "" || s.Label == "" {
			t.Errorf("%s: missing header text", view)
		}
		if len(s.Activity) != 40 {
			t.Errorf("%s: %d activity items, want 40", view, len(s.Activity))
		}
		for i := 1; i < len(s.Activity); i++ {
			if s.Activity[i].At.After(s.Activity[i-1].At) {
				t.Errorf("%s: activity not newest first at %d", view, i)
				break
			}
		}
		for _, p := range s.Peers {
			if p.Transport == "Tailscale" && !strings.HasPrefix(p.Addr, "100.") {
				t.Errorf("%s: synthetic tailnet peer outside 100.64/10: %s", view, p.Addr)
			}
		}
	}

	ch, cancel := d.SnapshotsFor("status")
	s := <-ch
	cancel()
	if up := countConnected(s.Peers); up < 2 {
		t.Errorf("status view should show '+N more': %d connected", up)
	}
	ch, cancel = d.SnapshotsFor("disconnected")
	s = <-ch
	cancel()
	if countConnected(s.Peers) != 0 || s.NeedBytes <= 0 || s.Headline != "Disconnected" {
		t.Errorf("disconnected view: up=%d need=%d headline=%q", countConnected(s.Peers), s.NeedBytes, s.Headline)
	}
	ch, cancel = d.SnapshotsFor("syncing")
	s = <-ch
	cancel()
	if s.Pct < 38 || s.Pct > 99 || !s.Moving || s.ETA == "" || s.InRate <= 1024 {
		t.Errorf("syncing view: pct=%d moving=%v eta=%q", s.Pct, s.Moving, s.ETA)
	}
	ch, cancel = d.SnapshotsFor("pair")
	s = <-ch
	cancel()
	if len(s.Pending) != 1 || s.Pending[0].Node == nil || s.Pending[0].Node.SameOwner || len(s.PendingFolders) != 1 {
		t.Errorf("pair view pending = %+v / %+v", s.Pending, s.PendingFolders)
	}
}

func countConnected(ps []model.Peer) int {
	n := 0
	for _, p := range ps {
		if p.Connected {
			n++
		}
	}
	return n
}

// TestDemoHandlesEveryAction keeps the demo backend in step with the action table.
func TestDemoHandlesEveryAction(t *testing.T) {
	d := Demo()
	args := map[string]string{
		"set-autostart":  `{"target":"syncthing","on":false}`,
		"pair":           `{"deviceID":"D6WQPLE-3KXTRZN"}`,
		"accept-device":  `{"deviceID":"Q2WMRTY"}`,
		"decline-device": `{"deviceID":"Q2WMRTY"}`,
		"share-folder":   `{"folderID":"docs-2k7qm","deviceIDs":["7QKX2PL"]}`,
		"accept-folder":  `{"folderID":"rcp-4n8wz","fromDevice":"M4TRV9C"}`,
		"decline-folder": `{"folderID":"rcp-4n8wz","fromDevice":"M4TRV9C"}`,
		"set-pref":       `{"key":"notifications","on":false}`,
		"set-profile":    `{"profile":"hybrid"}`,
		"dismiss-notice": `{"id":"gui-exposed","forever":true}`,
		"migrate-legacy": `{"yes":false}`,
		"open-folder":    `{"id":"phot-8x3vd"}`,
		"open-docs":      `{"page":"blocked"}`,
	}
	for _, name := range append(Actions(), "show", "quit") {
		var a json.RawMessage
		if s, ok := args[name]; ok {
			a = json.RawMessage(s)
		}
		res, err := d.Action(context.Background(), name, a)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := json.Marshal(res); err != nil {
			t.Errorf("%s: result not serialisable: %v", name, err)
		}
	}
	if res, _ := d.Action(context.Background(), "discover", nil); len(res.([]model.Candidate)) == 0 {
		t.Error("discover returned no candidates")
	}
	st, _ := d.Action(context.Background(), "get-settings", nil)
	if s := st.(demoSettings); s.Profile != "hybrid" || s.Notifications {
		t.Errorf("settings did not persist: %+v", s)
	}
	for name, a := range map[string]string{
		"set-profile":   `{"profile":"open"}`,
		"set-pref":      `{"key":"telemetry","on":true}`,
		"share-folder":  `{"folderID":"x","deviceIDs":[]}`,
		"set-autostart": `{"target":"everything","on":true}`,
		"open-folder":   `{"id":"nope"}`,
		"open-docs":     `{"page":"https://example.com"}`,
	} {
		if _, err := d.Action(context.Background(), name, json.RawMessage(a)); err == nil {
			t.Errorf("%s %s: expected an error", name, a)
		}
	}
	if _, err := d.Action(context.Background(), "no-such-action", nil); err == nil {
		t.Error("unknown action accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Action(ctx, "discover", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("discover ignores cancellation: %v", err)
	}
	if png := d.Backdrop(); len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Error("demo backdrop is not a PNG")
	}
}

// TestDemoServe serves the demo dashboard for manual checks:
//
//	go test ./internal/ui -run TestDemoServe -manual [-manual.port 18999] [-manual.minutes 15]
//
// It prints single-use login URLs (valid 30 s) for every demo view and
// refreshes them every 20 s. Open one in a browser; the session cookie then
// lets you switch views with ?demo=<view>&mode=browser.
func TestDemoServe(t *testing.T) {
	if !*manual {
		t.Skip("manual check; run with -manual")
	}
	s, err := NewServerPort(Demo(), *manualPort)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fmt.Fprintf(os.Stdout, "Demo dashboard on %s (control token %s)\n", s.URL(), s.ControlToken())
	print := func() {
		fmt.Fprintf(os.Stdout, "\n%s login URLs (single-use, 30 s):\n", time.Now().Format("15:04:05"))
		for _, v := range DemoViews {
			fmt.Fprintf(os.Stdout, "  %-13s %s\n", v, s.LoginURL(s.NewLaunchToken(), "/?demo="+v+"&mode=browser"))
		}
	}
	print()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	end := time.After(time.Duration(*manualMinute) * time.Minute)
	for {
		select {
		case <-tick.C:
			print()
		case <-stop:
			return
		case <-end:
			return
		}
	}
}

// TestActionErrorCarriesDocs: an error tagged with WithDocs reaches the page
// as {"docs":page}, so the Pair view can show the §4.2 help link.
func TestActionErrorCarriesDocs(t *testing.T) {
	f := newFake()
	s := startServer(t, f)
	sess := login(t, s)

	f.set(func() {
		f.err = WithDocs(errors.New("Tailscale is not connected. Open Tailscale and sign in."), "tailscale-not-connected")
	})
	resp, out := postAction(t, s, sess, `{"name":"discover"}`)
	if resp.StatusCode != http.StatusInternalServerError || out["ok"] != false ||
		out["error"] != "Tailscale is not connected. Open Tailscale and sign in." || out["docs"] != "tailscale-not-connected" {
		t.Fatalf("tagged error: %d %v", resp.StatusCode, out)
	}
	// Wrapping keeps the tag.
	f.set(func() { f.err = fmt.Errorf("discover: %w", WithDocs(errors.New("refused"), "blocked")) })
	if _, out := postAction(t, s, sess, `{"name":"discover"}`); out["docs"] != "blocked" {
		t.Fatalf("wrapped tagged error: %v", out)
	}
	// Untagged errors and unknown pages carry no docs field.
	for _, err := range []error{errors.New("plain"), WithDocs(errors.New("x"), "https://example.com")} {
		f.set(func() { f.err = err })
		if _, out := postAction(t, s, sess, `{"name":"discover"}`); out["docs"] != nil {
			t.Fatalf("%v: unexpected docs %v", err, out["docs"])
		}
	}
	if WithDocs(nil, "blocked") != nil {
		t.Fatal("WithDocs(nil) must stay nil")
	}
}

// TestDocsPagesResolve checks that every open-docs page points into this
// repository's docs/ folder at a file and heading that exist.
func TestDocsPagesResolve(t *testing.T) {
	pages := DocsPages()
	if !sort.StringsAreSorted(pages) || len(pages) < 3 {
		t.Fatalf("DocsPages() = %v", pages)
	}
	for _, want := range []string{"tailscale-not-connected", "blocked"} { // §4.2
		if _, ok := DocsURL(want); !ok {
			t.Errorf("§4.2 docs page %q missing", want)
		}
	}
	if _, ok := DocsURL("https://example.com"); ok {
		t.Error("DocsURL accepted a URL")
	}
	prefix := brand.RepoURL + "/blob/main/docs/"
	heading := regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	for _, page := range pages {
		u, _ := DocsURL(page)
		rel, ok := strings.CutPrefix(u, prefix)
		if !ok {
			t.Errorf("%s: %q is not under %s", page, u, prefix)
			continue
		}
		file, anchor, _ := strings.Cut(rel, "#")
		src, err := os.ReadFile(filepath.Join("..", "..", "docs", filepath.FromSlash(file)))
		if err != nil {
			t.Errorf("%s: %v", page, err)
			continue
		}
		if anchor == "" {
			continue
		}
		found := false
		for _, m := range heading.FindAllStringSubmatch(string(src), -1) {
			if githubSlug(m[1]) == anchor {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: docs/%s has no heading for #%s", page, file, anchor)
		}
	}
}

// githubSlug is GitHub's heading anchor: lower case, punctuation dropped,
// spaces as hyphens.
func githubSlug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(h)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
