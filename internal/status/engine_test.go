package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

const (
	fakeKey = "synthetic-engine-key"
	myID    = "MYSELF0-MYSELF0-MYSELF0-MYSELF0-MYSELF0-MYSELF0-MYSELF0-MYSELF0"
)

// evResp is one scripted answer to a since=N long-poll.
type evResp struct {
	status int
	events string // JSON array
}

// fakeST is an in-process Syncthing REST and event server with synthetic data.
type fakeST struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	authStatus  int // non-zero: every request gets this status
	devices     string
	folders     string
	dbStatus    map[string]string
	connections string
	pendingDev  string
	pendingFold string
	seedID      int
	seeds       int
	sinces      []int
	script      []evResp
	requests    []string
	patches     map[string]string
	failPath    string
}

func newFakeST(t *testing.T) *fakeST {
	f := &fakeST{
		t: t,
		devices: `[{"deviceID":"` + myID + `","name":"this-device"},
			{"deviceID":"` + idA + `","name":"example-b"},
			{"deviceID":"` + idB + `","name":""}]`,
		folders: `[{"id":"exmpl-00001","label":"Example","path":"/srv/example","paused":false},
			{"id":"exmpl-00002","label":"","path":"/srv/second","paused":false}]`,
		dbStatus: map[string]string{
			"exmpl-00001": `{"globalBytes":1000,"needBytes":0,"inSyncBytes":1000,"globalFiles":10,"localFiles":10,"state":"idle"}`,
			"exmpl-00002": `{"globalBytes":3000,"needBytes":0,"inSyncBytes":3000,"globalFiles":5,"localFiles":5,"state":"idle"}`,
		},
		connections: `{"total":{"inBytesTotal":0,"outBytesTotal":0},"connections":{
			"` + idA + `":{"connected":true,"address":"100.64.0.2:22000","type":"tcp-client"},
			"` + idB + `":{"connected":false,"address":"","type":""}}}`,
		pendingDev:  `{}`,
		pendingFold: `{}`,
		seedID:      10,
		patches:     map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeST) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("X-API-Key") != fakeKey {
		f.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.authStatus != 0 {
		st := f.authStatus
		f.mu.Unlock()
		w.WriteHeader(st)
		return
	}
	if f.failPath != "" && r.URL.Path == f.failPath {
		f.mu.Unlock()
		http.Error(w, "synthetic failure", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	var body string
	switch {
	case r.URL.Path == "/rest/events":
		if q.Get("limit") == "1" {
			f.seeds++
			body = fmt.Sprintf(`[{"id":%d,"type":"StartupComplete","time":"2026-01-02T03:04:05Z","data":{}}]`, f.seedID)
			break
		}
		since := 0
		fmt.Sscan(q.Get("since"), &since)
		f.sinces = append(f.sinces, since)
		if len(f.script) == 0 {
			f.mu.Unlock()
			// Hold like a real long-poll, briefly, then report nothing.
			select {
			case <-r.Context().Done():
			case <-time.After(50 * time.Millisecond):
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		resp := f.script[0]
		f.script = f.script[1:]
		f.mu.Unlock()
		if resp.status != 0 && resp.status != http.StatusOK {
			http.Error(w, "synthetic", resp.status)
			return
		}
		_, _ = w.Write([]byte(resp.events))
		return
	case r.URL.Path == "/rest/system/status":
		body = `{"myID":"` + myID + `"}`
	case r.URL.Path == "/rest/config/devices":
		body = f.devices
	case r.URL.Path == "/rest/config/folders" && r.Method == http.MethodGet:
		body = f.folders
	case strings.HasPrefix(r.URL.Path, "/rest/config/folders/") && r.Method == http.MethodPatch:
		b, _ := io.ReadAll(r.Body)
		id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/rest/config/folders/"))
		f.patches[id] = string(b)
	case r.URL.Path == "/rest/db/status":
		s, ok := f.dbStatus[q.Get("folder")]
		if !ok {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		body = s
	case r.URL.Path == "/rest/system/connections":
		body = f.connections
	case r.URL.Path == "/rest/cluster/pending/devices":
		body = f.pendingDev
	case r.URL.Path == "/rest/cluster/pending/folders":
		body = f.pendingFold
	case r.Method == http.MethodPost && (r.URL.Path == "/rest/db/scan" || r.URL.Path == "/rest/system/restart"):
	default:
		f.mu.Unlock()
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
		return
	}
	f.mu.Unlock()
	if body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeST) set(fn func(f *fakeST)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeST) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeST) client(t *testing.T) *stclient.Client {
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return stclient.New(stclient.Endpoint{BaseURL: u, APIKey: fakeKey})
}

// testEngine returns an engine with fast timings against f.
func testEngine(t *testing.T, c *stclient.Client, startup func() model.Startup) *Engine {
	e := NewEngine(c, startup)
	e.pollEvery = 20 * time.Millisecond
	e.slowEvery = time.Hour
	e.backoff = 10 * time.Millisecond
	e.eventsTimeout = 2 * time.Second
	return e
}

func run(t *testing.T, e *Engine) <-chan model.Snapshot {
	ctx, cancel := context.WithCancel(context.Background())
	ch := e.Run(ctx)
	t.Cleanup(func() {
		cancel()
		for range ch { // the channel closes once the engine stopped
		}
	})
	return ch
}

func waitFor(t *testing.T, ch <-chan model.Snapshot, what string, ok func(model.Snapshot) bool) model.Snapshot {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var last model.Snapshot
	for {
		select {
		case s := <-ch:
			last = s
			if ok(s) {
				return s
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last snapshot: state=%s folder=%q peers=%q activity=%v",
				what, Label(last.State), last.FolderLine, last.PeerLine, last.Activity)
		}
	}
}

func hasActivity(s model.Snapshot, text string) bool {
	for _, a := range s.Activity {
		if a.Text == text {
			return true
		}
	}
	return false
}

func TestEngineStartup(t *testing.T) {
	f := newFakeST(t)
	f.set(func(f *fakeST) {
		f.pendingDev = `{"` + idB + `":{"time":"2026-01-02T03:04:05Z","name":"example-c","address":"100.64.0.3:22000"}}`
		f.pendingFold = `{"offer-001":{"offeredBy":{"` + idA + `":{"time":"2026-01-02T03:04:05Z","label":"Offered"}}}}`
	})
	startup := func() model.Startup { return model.Startup{Syncthing: true, Tray: true} }
	ch := run(t, testEngine(t, f.client(t), startup))

	s := waitFor(t, ch, "in sync", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	if s.PeerLine != "example-b (Tailscale)" || s.FolderLine != "Up to date" || s.Subline != "All devices hold the same files." {
		t.Errorf("lines: %q %q %q", s.PeerLine, s.FolderLine, s.Subline)
	}
	if len(s.Peers) != 2 || s.Peers[0].ID != idA || !s.Peers[0].Connected || s.Peers[0].Transport != TransportTailscale ||
		s.Peers[1].Name != "BBBBBBB" || s.Peers[1].Connected {
		t.Errorf("peers = %+v", s.Peers)
	}
	if len(s.Folders) != 2 || s.Folders[1].Label != "exmpl-00002" || s.Folders[0].GlobalBytes != 1000 {
		t.Errorf("folders = %+v", s.Folders)
	}
	if s.FilesLine != "15 of 15 files" || s.SizeLine != "3.9 KB of 3.9 KB in sync" || s.Pct != 100 {
		t.Errorf("files %q size %q pct %d", s.FilesLine, s.SizeLine, s.Pct)
	}
	if s.GUIURL != f.srv.URL+"/" || strings.Contains(s.GUIURL, fakeKey) || !s.Startup.Syncthing || s.At.IsZero() {
		t.Errorf("gui %q startup %+v at %v", s.GUIURL, s.Startup, s.At)
	}
	if len(s.Pending) != 1 || s.Pending[0].Name != "example-c" || s.Pending[0].Addr.String() != "100.64.0.3:22000" ||
		len(s.PendingFolders) != 1 || s.PendingFolders[0] != (model.PendingFolder{FolderID: "offer-001", Label: "Offered", FromDevice: idA}) {
		t.Errorf("pending %+v %+v", s.Pending, s.PendingFolders)
	}
	for _, want := range []string{"GET /rest/system/status", "GET /rest/config/devices", "GET /rest/config/folders",
		"GET /rest/db/status?folder=exmpl-00001", "GET /rest/db/status?folder=exmpl-00002", "GET /rest/system/connections"} {
		if f.count(want) == 0 {
			t.Errorf("no %s", want)
		}
	}

	// The 2 s poll only touches connections; db/status is not polled.
	dbBefore := f.count("GET /rest/db/status")
	connBefore := f.count("GET /rest/system/connections")
	time.Sleep(200 * time.Millisecond)
	if f.count("GET /rest/system/connections") <= connBefore+2 {
		t.Error("connections are not polled")
	}
	if got := f.count("GET /rest/db/status"); got != dbBefore {
		t.Errorf("db/status polled %d times without events", got-dbBefore)
	}
}

func TestEngineReconcileTicker(t *testing.T) {
	f := newFakeST(t)
	e := testEngine(t, f.client(t), nil)
	e.slowEvery = 30 * time.Millisecond
	ch := run(t, e)
	waitFor(t, ch, "in sync", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	time.Sleep(200 * time.Millisecond)
	if n := f.count("GET /rest/db/status?folder=exmpl-00001"); n < 3 {
		t.Errorf("reconcile ran %d times", n)
	}
	if n := f.count("GET /rest/cluster/pending/devices"); n < 3 {
		t.Errorf("pending polled %d times", n)
	}
}

func TestEngineEventReseedAfterServerError(t *testing.T) {
	f := newFakeST(t)
	f.set(func(f *fakeST) {
		f.seedID = 10
		f.script = []evResp{{status: http.StatusInternalServerError}}
	})
	ch := run(t, testEngine(t, f.client(t), nil))
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })

	// After the 500 the engine must seed again before long-polling.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		seeds := f.seeds
		f.mu.Unlock()
		if seeds >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.set(func(f *fakeST) {
		if f.seeds < 2 {
			t.Fatalf("seeds = %d after a server error, want a re-seed", f.seeds)
		}
		if len(f.sinces) == 0 || f.sinces[0] != 10 {
			t.Errorf("first long-poll since = %v, want 10", f.sinces)
		}
		f.script = append(f.script, evResp{events: `[{"id":11,"type":"DeviceConnected","time":"2026-01-02T03:04:06Z","data":{}},
			{"id":12,"type":"ItemFinished","time":"2026-01-02T03:04:07Z","data":{"item":"dir/new.txt","action":"update","error":null}}]`})
	})
	s := waitFor(t, ch, "event activity", func(s model.Snapshot) bool {
		return hasActivity(s, "Device connected") && hasActivity(s, "Updated new.txt")
	})
	if s.Activity[0].Text != "Updated new.txt" || s.Activity[0].Tint != TintBlue {
		t.Errorf("newest activity = %+v", s.Activity[0])
	}
	if hasActivity(s, "StartupComplete") {
		t.Error("seed event reported")
	}
	// Later long-polls continue from the newest ID.
	time.Sleep(100 * time.Millisecond)
	f.set(func(f *fakeST) {
		if last := f.sinces[len(f.sinces)-1]; last != 12 {
			t.Errorf("since after batch = %d (all %v), want 12", last, f.sinces)
		}
	})
}

func TestEngineDecodeErrorKeepsSince(t *testing.T) {
	f := newFakeST(t)
	f.set(func(f *fakeST) { f.script = []evResp{{events: `[{"id":`}} })
	ch := run(t, testEngine(t, f.client(t), nil))
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	time.Sleep(150 * time.Millisecond)
	f.set(func(f *fakeST) {
		if f.seeds != 1 {
			t.Errorf("seeds = %d after a malformed batch, want 1", f.seeds)
		}
		if len(f.sinces) < 2 || f.sinces[1] != 10 {
			t.Errorf("sinces = %v, want a retry from 10", f.sinces)
		}
	})
}

func TestEngineFolderEvents(t *testing.T) {
	f := newFakeST(t)
	ch := run(t, testEngine(t, f.client(t), nil))
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	db2 := f.count("GET /rest/db/status?folder=exmpl-00002")

	// FolderSummary carries the numbers itself: no fetch.
	f.set(func(f *fakeST) {
		f.script = append(f.script, evResp{events: `[{"id":11,"type":"FolderSummary","data":{"folder":"exmpl-00001",
			"summary":{"globalBytes":4000,"needBytes":1000,"inSyncBytes":3000,"globalFiles":10,"localFiles":9,"needFiles":2,"needDeletes":1,"state":"syncing"}}}]`})
	})
	s := waitFor(t, ch, "syncing from summary", func(s model.Snapshot) bool { return s.State == model.StateSyncing })
	if s.FolderLine != "85% - 1000 B left" || s.NeedItems != 3 {
		t.Errorf("after summary: %q items %d", s.FolderLine, s.NeedItems)
	}
	if f.count("GET /rest/db/status?folder=exmpl-00001") != 1 {
		t.Error("FolderSummary triggered a db/status fetch")
	}

	// StateChanged fetches that folder only.
	f.set(func(f *fakeST) {
		f.dbStatus["exmpl-00001"] = `{"globalBytes":4000,"needBytes":0,"inSyncBytes":4000,"globalFiles":10,"localFiles":10,"state":"idle"}`
		f.script = append(f.script, evResp{events: `[{"id":12,"type":"StateChanged","data":{"folder":"exmpl-00001","from":"syncing","to":"idle"}}]`})
	})
	s = waitFor(t, ch, "in sync after state change", func(s model.Snapshot) bool {
		return s.State == model.StateInSync && hasActivity(s, "Up to date")
	})
	if f.count("GET /rest/db/status?folder=exmpl-00001") != 2 || f.count("GET /rest/db/status?folder=exmpl-00002") != db2 {
		t.Errorf("StateChanged fetches: folder1 %d folder2 %d (was %d)",
			f.count("GET /rest/db/status?folder=exmpl-00001"), f.count("GET /rest/db/status?folder=exmpl-00002"), db2)
	}
	if s.SizeLine != "6.8 KB of 6.8 KB in sync" {
		t.Errorf("size line %q", s.SizeLine)
	}
}

func TestEngineConfigSaved(t *testing.T) {
	f := newFakeST(t)
	ch := run(t, testEngine(t, f.client(t), nil))
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })

	f.set(func(f *fakeST) {
		f.folders = `[{"id":"exmpl-00001","label":"Example","path":"/srv/example","paused":false},
			{"id":"exmpl-00003","label":"Third","path":"/srv/third","paused":false}]`
		f.dbStatus["exmpl-00003"] = `{"globalBytes":10,"needBytes":0,"inSyncBytes":10,"globalFiles":1,"localFiles":1,"state":"scanning"}`
		f.script = append(f.script, evResp{events: `[{"id":11,"type":"ConfigSaved","data":{}}]`})
	})
	s := waitFor(t, ch, "config reload", func(s model.Snapshot) bool {
		return len(s.Folders) == 2 && s.Folders[1].ID == "exmpl-00003" && s.State == model.StateScanning
	})
	if !hasActivity(s, "Settings updated") || s.Folders[1].Label != "Third" {
		t.Errorf("activity %v folders %+v", s.Activity, s.Folders)
	}
}

func TestEngineDisconnectedWithNeed(t *testing.T) {
	f := newFakeST(t)
	f.set(func(f *fakeST) {
		f.dbStatus["exmpl-00001"] = `{"globalBytes":4096,"needBytes":1024,"inSyncBytes":3072,"state":"idle"}`
		f.connections = `{"total":{"inBytesTotal":0,"outBytesTotal":0},"connections":{"` + idA + `":{"connected":false}}}`
	})
	ch := run(t, testEngine(t, f.client(t), nil))
	s := waitFor(t, ch, "disconnected", func(s model.Snapshot) bool { return s.State == model.StateNoPeer })
	if s.Headline != "Disconnected" || s.FolderLine != "85% - 1.0 KB left" || s.PeerLine != "No devices connected" {
		t.Errorf("disconnected lines: %q %q %q", s.Headline, s.FolderLine, s.PeerLine)
	}
}

func TestEngineUnauthorizedAndDown(t *testing.T) {
	f := newFakeST(t)
	f.set(func(f *fakeST) { f.authStatus = http.StatusUnauthorized })
	ch := run(t, testEngine(t, f.client(t), nil))
	s := waitFor(t, ch, "unauthorized", func(s model.Snapshot) bool { return s.State == model.StateUnauthorized })
	if s.Detail != "Syncthing rejected the API key — run stv2 doctor" || s.Peers != nil {
		t.Errorf("unauthorized snapshot: %q %+v", s.Detail, s.Peers)
	}

	// Recovery performs the full startup reads again.
	f.set(func(f *fakeST) { f.authStatus = 0 })
	waitFor(t, ch, "recovered", func(s model.Snapshot) bool { return s.State == model.StateInSync && len(s.Folders) == 2 })

	f.set(func(f *fakeST) { f.authStatus = http.StatusForbidden })
	waitFor(t, ch, "forbidden", func(s model.Snapshot) bool { return s.State == model.StateUnauthorized })

	f.set(func(f *fakeST) { f.authStatus = http.StatusBadGateway })
	waitFor(t, ch, "bad response", func(s model.Snapshot) bool {
		return s.State == model.StateError && s.Detail == "Unexpected response from Syncthing"
	})

	f.srv.Close()
	s = waitFor(t, ch, "down", func(s model.Snapshot) bool { return s.State == model.StateDown })
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if s.Detail != "Syncthing is not responding on "+host || s.Label != "Not running" {
		t.Errorf("down snapshot: %q %q", s.Detail, s.Label)
	}
}

// A read that keeps failing with an unexpected response holds the Error state
// across the fast connections polls instead of flickering back, and the
// engine recovers within a fast tick once the read works again.
func TestEnginePersistentReadFailureStaysError(t *testing.T) {
	for _, c := range []struct {
		name, path string
		trigger    func(f *fakeST, e *Engine)
	}{
		{"db/status on StateChanged", "/rest/db/status", func(f *fakeST, e *Engine) {
			f.set(func(f *fakeST) {
				f.script = append(f.script, evResp{events: `[{"id":11,"type":"StateChanged","data":{"folder":"exmpl-00001","from":"idle","to":"scanning"}}]`})
			})
		}},
		{"db/status on reconcile", "/rest/db/status", func(f *fakeST, e *Engine) { e.Refresh() }},
		{"system/status on reconcile", "/rest/system/status", func(f *fakeST, e *Engine) { e.Refresh() }},
		{"pending devices", "/rest/cluster/pending/devices", func(f *fakeST, e *Engine) { e.Refresh() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeST(t)
			e := testEngine(t, f.client(t), nil)
			ch := run(t, e)
			waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })

			f.set(func(f *fakeST) { f.failPath = c.path })
			c.trigger(f, e)
			waitFor(t, ch, "error", func(s model.Snapshot) bool {
				return s.State == model.StateError && s.Detail == "Unexpected response from Syncthing"
			})

			// Many 20 ms fast ticks, each polling connections successfully.
			conns := f.count("GET /rest/system/connections")
			end := time.After(300 * time.Millisecond)
			seen := 0
		watch:
			for {
				select {
				case s := <-ch:
					seen++
					if s.State != model.StateError {
						t.Fatalf("state fell back to %s while %s keeps failing", Label(s.State), c.path)
					}
				case <-end:
					break watch
				}
			}
			if seen < 5 {
				t.Errorf("only %d snapshots while failing; the fast ticks did not run", seen)
			}
			if c.path != "/rest/system/status" && f.count("GET /rest/system/connections") < conns+1 {
				t.Error("connections were not polled while the read kept failing")
			}

			f.set(func(f *fakeST) { f.failPath = "" })
			waitFor(t, ch, "recovery", func(s model.Snapshot) bool { return s.State == model.StateInSync })
		})
	}
}

// A failing connections poll alone is cleared by the next good poll.
func TestEngineConnectionsFailureRecovers(t *testing.T) {
	f := newFakeST(t)
	ch := run(t, testEngine(t, f.client(t), nil))
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	f.set(func(f *fakeST) { f.failPath = "/rest/system/connections" })
	waitFor(t, ch, "error", func(s model.Snapshot) bool { return s.State == model.StateError })
	status := f.count("GET /rest/system/status")
	f.set(func(f *fakeST) { f.failPath = "" })
	s := waitFor(t, ch, "recovery", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	if len(s.Peers) != 2 || !s.Peers[0].Connected {
		t.Errorf("peers after recovery = %+v", s.Peers)
	}
	if f.count("GET /rest/system/status") != status {
		t.Error("a connections failure triggered the full startup reads")
	}
}

func TestEngineDownFromStart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	u, _ := url.Parse("http://" + addr)
	e := testEngine(t, stclient.New(stclient.Endpoint{BaseURL: u, APIKey: fakeKey}), nil)
	ch := run(t, e)
	s := waitFor(t, ch, "down", func(s model.Snapshot) bool { return s.State == model.StateDown })
	if s.Detail != "Syncthing is not responding on "+addr || s.Headline != "Syncthing not running" {
		t.Errorf("down: %q %q", s.Detail, s.Headline)
	}
}

func TestEngineCommands(t *testing.T) {
	f := newFakeST(t)
	e := testEngine(t, f.client(t), nil)
	ch := run(t, e)
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	ctx := context.Background()

	statusBefore := f.count("GET /rest/system/status")
	if err := e.Rescan(ctx); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, ch, "rescan note", func(s model.Snapshot) bool { return hasActivity(s, "Rescan requested") })
	if s.Activity[0].Tint != TintAmber || f.count("POST /rest/db/scan") != 1 {
		t.Errorf("rescan: %+v, posts %d", s.Activity[0], f.count("POST /rest/db/scan"))
	}
	waitFor(t, ch, "reconcile after command", func(model.Snapshot) bool { return f.count("GET /rest/system/status") > statusBefore })

	if err := e.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	s = waitFor(t, ch, "pause note", func(s model.Snapshot) bool { return hasActivity(s, "Syncing paused") })
	if s.Activity[0].Tint != TintGrey {
		t.Errorf("pause tint %q", s.Activity[0].Tint)
	}
	f.set(func(f *fakeST) {
		if f.patches["exmpl-00001"] != `{"paused":true}` || f.patches["exmpl-00002"] != `{"paused":true}` {
			t.Errorf("pause patches = %v", f.patches)
		}
	})

	if err := e.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	s = waitFor(t, ch, "resume note", func(s model.Snapshot) bool { return hasActivity(s, "Syncing resumed") })
	if s.Activity[0].Tint != TintGreen {
		t.Errorf("resume tint %q", s.Activity[0].Tint)
	}
	f.set(func(f *fakeST) {
		if f.patches["exmpl-00001"] != `{"paused":false}` {
			t.Errorf("resume patches = %v", f.patches)
		}
		f.failPath = "/rest/system/restart"
	})

	err := e.Restart(ctx)
	if k, _ := stclient.KindOf(err); err == nil || k != stclient.ErrBadResponse {
		t.Fatalf("Restart = %v, want a bad response error", err)
	}
	s = waitFor(t, ch, "failure note", func(s model.Snapshot) bool {
		return hasActivity(s, "Command failed: unexpected response from Syncthing (HTTP 500)")
	})
	if s.Activity[0].Tint != TintRed {
		t.Errorf("failure tint %q", s.Activity[0].Tint)
	}

	f.set(func(f *fakeST) { f.failPath = "/rest/config/folders/exmpl-00001" })
	if err := e.Pause(ctx); err == nil {
		t.Fatal("Pause succeeded against a failing server")
	}
	waitFor(t, ch, "pause failure note", func(s model.Snapshot) bool {
		return hasActivity(s, "Pause failed: unexpected response from Syncthing (HTTP 500)")
	})

	e.Note("Starting Syncthing", TintAmber)
	s = waitFor(t, ch, "manual note", func(s model.Snapshot) bool { return hasActivity(s, "Starting Syncthing") })
	if s.Activity[0].Text != "Starting Syncthing" {
		t.Errorf("note is not newest: %+v", s.Activity[0])
	}
}

func TestEngineActivityCap(t *testing.T) {
	f := newFakeST(t)
	e := testEngine(t, f.client(t), nil)
	for i := range MaxActivity + 5 {
		e.Note(fmt.Sprintf("note %d", i), TintGrey)
	}
	ch := run(t, e)
	s := waitFor(t, ch, "notes", func(s model.Snapshot) bool { return len(s.Activity) == MaxActivity })
	if s.Activity[0].Text != fmt.Sprintf("note %d", MaxActivity+4) || s.Activity[MaxActivity-1].Text != "note 5" {
		t.Errorf("activity order: first %q last %q", s.Activity[0].Text, s.Activity[MaxActivity-1].Text)
	}
}

func TestEngineRates(t *testing.T) {
	f := newFakeST(t)
	e := testEngine(t, f.client(t), nil)
	e.pollEvery = 30 * time.Millisecond
	var mu sync.Mutex
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { // every call advances 2 s, like the production poll
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(2 * time.Second)
		return clock
	}
	var in int64
	f.set(func(f *fakeST) { f.connections = connJSON(0) })
	ch := run(t, e)
	waitFor(t, ch, "startup", func(s model.Snapshot) bool { return s.State == model.StateInSync })
	go func() {
		for range 20 {
			time.Sleep(15 * time.Millisecond)
			in += 1 << 20
			v := in
			f.set(func(f *fakeST) { f.connections = connJSON(v) })
		}
	}()
	s := waitFor(t, ch, "moving", func(s model.Snapshot) bool { return s.Moving })
	if s.InRate <= 1024 || !strings.HasPrefix(s.SpeedLine, "v ") {
		t.Errorf("rates %v/%v speed %q", s.InRate, s.OutRate, s.SpeedLine)
	}
}

func connJSON(in int64) string {
	b, _ := json.Marshal(map[string]any{
		"total": map[string]int64{"inBytesTotal": in, "outBytesTotal": 0},
		"connections": map[string]any{
			idA: map[string]any{"connected": true, "address": "100.64.0.2:22000", "type": "tcp-client"},
		},
	})
	return string(b)
}
