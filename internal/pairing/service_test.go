package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

// Synthetic device IDs.
var (
	idSelf   = deviceid.FromCert([]byte("synthetic self"))
	idLaptop = deviceid.FromCert([]byte("synthetic laptop"))
	idMac    = deviceid.FromCert([]byte("synthetic mac"))
	idFriend = deviceid.FromCert([]byte("synthetic friend"))
	idOther  = deviceid.FromCert([]byte("synthetic other"))
	idLAN    = deviceid.FromCert([]byte("synthetic lan"))
)

const apiKey = "synthetic-api-key"

// fakeST is an in-memory Syncthing REST API covering the endpoints the
// pairing package uses.
type fakeST struct {
	t  *testing.T
	mu sync.Mutex

	myID           string
	devices        []map[string]any
	folders        map[string]map[string]any
	pendingDevices map[string]pendingDeviceJSON
	pendingFolders map[string]map[string]string // folder → device → label
	writes         []string                     // "METHOD path?query"
	bodies         []map[string]any
}

func newFakeST(t *testing.T) (*fakeST, *stclient.Client) {
	f := &fakeST{
		t:              t,
		myID:           idSelf,
		devices:        []map[string]any{{"deviceID": idSelf, "name": "this-computer", "addresses": []any{"dynamic"}}},
		folders:        map[string]map[string]any{},
		pendingDevices: map[string]pendingDeviceJSON{},
		pendingFolders: map[string]map[string]string{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, stclient.New(stclient.Endpoint{BaseURL: u, APIKey: apiKey})
}

func (f *fakeST) device(id string) map[string]any {
	for _, d := range f.devices {
		if d["deviceID"] == id {
			return d
		}
	}
	return nil
}

func (f *fakeST) addDevice(d map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = append(f.devices, d)
}

func (f *fakeST) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeST) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != apiKey {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var body map[string]any
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.RequestURI())
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		f.bodies = append(f.bodies, body)
	}

	p := r.URL.Path
	switch {
	case p == "/rest/system/status" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"myID": f.myID})

	case p == "/rest/config/devices" && r.Method == http.MethodGet:
		writeJSON(w, f.devices)

	case strings.HasPrefix(p, "/rest/config/devices/"):
		id := strings.TrimPrefix(p, "/rest/config/devices/")
		d := f.device(id)
		switch r.Method {
		case http.MethodGet:
			if d == nil {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, d)
		case http.MethodPut:
			if body["deviceID"] != id {
				http.Error(w, "deviceID mismatch", http.StatusBadRequest)
				return
			}
			if d == nil {
				f.devices = append(f.devices, body)
			} else {
				for k := range d {
					delete(d, k)
				}
				for k, v := range body {
					d[k] = v
				}
			}
		case http.MethodPatch:
			if d == nil {
				http.NotFound(w, r)
				return
			}
			for k, v := range body {
				d[k] = v
			}
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	case strings.HasPrefix(p, "/rest/config/folders/"):
		id := strings.TrimPrefix(p, "/rest/config/folders/")
		switch r.Method {
		case http.MethodGet:
			fo, ok := f.folders[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, fo)
		case http.MethodPut:
			if body["id"] != id {
				http.Error(w, "id mismatch", http.StatusBadRequest)
				return
			}
			f.folders[id] = body
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	case p == "/rest/cluster/pending/devices":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.pendingDevices)
		case http.MethodDelete:
			delete(f.pendingDevices, r.URL.Query().Get("device"))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	case p == "/rest/cluster/pending/folders":
		q := r.URL.Query()
		switch r.Method {
		case http.MethodGet:
			out := map[string]pendingFolderJSON{}
			for fid, by := range f.pendingFolders {
				for dev, label := range by {
					if q.Get("device") != "" && q.Get("device") != dev {
						continue
					}
					e := out[fid]
					if e.OfferedBy == nil {
						e.OfferedBy = map[string]struct {
							Label string `json:"label"`
						}{}
					}
					e.OfferedBy[dev] = struct {
						Label string `json:"label"`
					}{label}
					out[fid] = e
				}
			}
			writeJSON(w, out)
		case http.MethodDelete:
			delete(f.pendingFolders[q.Get("folder")], q.Get("device"))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	default:
		http.NotFound(w, r)
	}
}

// fakeTS is a tailnet.Source returning a fixed status.
type fakeTS struct {
	st    tailnet.Status
	err   error
	calls atomic.Int32
}

func (f *fakeTS) Status(context.Context) (tailnet.Status, error) {
	f.calls.Add(1)
	return f.st, f.err
}

// testTailnet: self (user 1) and peers laptop, mac (user 1), friend (user 2),
// plus nodes excluded from discovery.
func testTailnet() tailnet.Status {
	ip := func(s ...string) []netip.Addr {
		var out []netip.Addr
		for _, x := range s {
			out = append(out, netip.MustParseAddr(x))
		}
		return out
	}
	return tailnet.Status{
		BackendState: "Running",
		Self:         tailnet.Node{ID: "n0", HostName: "this-computer", OS: "windows", UserID: 1, LoginName: "owner@example.com", IPs: ip("100.64.0.1"), Online: true},
		Peers: []tailnet.Node{
			{ID: "n2", HostName: "laptop", DNSName: "laptop.tailnet-example.ts.net", OS: "windows", UserID: 1, LoginName: "owner@example.com", IPs: ip("100.64.0.2", "fd7a:115c:a1e0::2"), Online: true},
			{ID: "n3", HostName: "mac", OS: "macOS", UserID: 1, LoginName: "owner@example.com", IPs: ip("fd7a:115c:a1e0::3", "100.64.0.3"), Online: true},
			{ID: "n8", HostName: "friend-pc", OS: "linux", UserID: 2, LoginName: "friend@example.org", IPs: ip("100.64.0.8"), Online: true},
			{ID: "n9", HostName: "old-box", OS: "linux", UserID: 1, IPs: ip("100.64.0.9"), Online: true},
			{ID: "n10", HostName: "twin", OS: "linux", UserID: 1, IPs: ip("100.64.0.10"), Online: true},
			{ID: "n11", HostName: "firewalled", OS: "linux", UserID: 1, IPs: ip("100.64.0.11"), Online: true},
			{ID: "n12", HostName: "phone", OS: "android", UserID: 1, IPs: ip("100.64.0.12"), Online: true},
			{ID: "n13", HostName: "tagged", OS: "linux", UserID: 1, IPs: ip("100.64.0.13"), Online: true, Tags: []string{"tag:ci"}},
			{ID: "n14", HostName: "offline", OS: "linux", UserID: 1, IPs: ip("100.64.0.14"), Online: false},
		},
	}
}

// probeTable returns a probe function answering from a table keyed by IP,
// counting calls and tracking the maximum concurrency.
type probeTable struct {
	mu       sync.Mutex
	results  map[string]probeResult
	calls    map[string]int
	inFlight int
	maxIn    int
	delay    time.Duration
	deadline []time.Duration
}

type probeResult struct {
	id  string
	err error
}

func newProbeTable(r map[string]probeResult) *probeTable {
	return &probeTable{results: r, calls: map[string]int{}}
}

func (p *probeTable) probe(ctx context.Context, ap netip.AddrPort) (string, error) {
	p.mu.Lock()
	p.calls[ap.String()]++
	p.inFlight++
	if p.inFlight > p.maxIn {
		p.maxIn = p.inFlight
	}
	if dl, ok := ctx.Deadline(); ok {
		p.deadline = append(p.deadline, time.Until(dl))
	}
	r, ok := p.results[ap.Addr().String()]
	p.mu.Unlock()
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	if ap.Port() != SyncPort {
		return "", fmt.Errorf("probe on port %d, want %d", ap.Port(), SyncPort)
	}
	if !ok {
		return "", ErrTimeout
	}
	return r.id, r.err
}

func (p *probeTable) count(ip string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[netip.AddrPortFrom(netip.MustParseAddr(ip), SyncPort).String()]
}

func newService(c *stclient.Client, ts tailnet.Source, pt *probeTable, ps *prefs.Store) *Service {
	return &Service{C: c, TS: ts, Policy: Policy{Prefixes: tailnet.Prefixes}, Probe: pt.probe, Prefs: ps}
}

func TestDiscover(t *testing.T) {
	st, c := newFakeST(t)
	st.addDevice(map[string]any{"deviceID": idMac, "name": "mac"})
	pt := newProbeTable(map[string]probeResult{
		"100.64.0.2":  {id: idLaptop},
		"100.64.0.3":  {id: idMac},
		"100.64.0.8":  {id: idFriend},
		"100.64.0.9":  {err: ErrNoSyncthing},
		"100.64.0.10": {id: idSelf},
		"100.64.0.11": {err: ErrRefused},
	})
	pt.delay = 30 * time.Millisecond
	s := newService(c, &fakeTS{st: testTailnet()}, pt, nil)

	got, err := s.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ name, status, id, ip string }
	var rows []row
	for _, cd := range got {
		rows = append(rows, row{cd.NodeName, cd.Status, cd.DeviceID, cd.IP.String()})
	}
	want := []row{
		{"firewalled", StatusBlocked, "", "100.64.0.11"},
		{"laptop", StatusReady, idLaptop, "100.64.0.2"},
		{"mac", StatusPaired, idMac, "100.64.0.3"},
		{"old-box", StatusNoSyncthing, "", "100.64.0.9"},
		{"twin", StatusSelf, idSelf, "100.64.0.10"},
		{"friend-pc", StatusReady, idFriend, "100.64.0.8"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("Discover:\n got %v\nwant %v", rows, want)
	}
	if !got[1].SameOwner || got[5].SameOwner || got[5].LoginName != "friend@example.org" {
		t.Errorf("owner flags: %+v / %+v", got[1], got[5])
	}
	if got[1].DNSName != "laptop.tailnet-example.ts.net" || got[1].OS != "windows" {
		t.Errorf("laptop = %+v", got[1])
	}
	// Phone, tagged and offline nodes are never probed.
	for _, ip := range []string{"100.64.0.12", "100.64.0.13", "100.64.0.14"} {
		if n := pt.count(ip); n != 0 {
			t.Errorf("%s probed %d times", ip, n)
		}
	}
	if pt.maxIn > MaxParallelProbes || pt.maxIn < 2 {
		t.Errorf("max concurrent probes = %d, want 2..%d", pt.maxIn, MaxParallelProbes)
	}
	for _, d := range pt.deadline {
		if d > ProbeTimeout || d <= 0 {
			t.Errorf("probe deadline %v, want within %v", d, ProbeTimeout)
		}
	}
	if len(pt.deadline) != 6 {
		t.Errorf("%d probes carried a deadline, want 6", len(pt.deadline))
	}
	if w := st.writeLog(); len(w) != 0 {
		t.Errorf("Discover wrote to Syncthing: %v", w)
	}
}

func TestDiscoverTailscaleUnavailable(t *testing.T) {
	_, c := newFakeST(t)
	pt := newProbeTable(nil)

	down := testTailnet()
	down.BackendState = "NeedsLogin"
	if _, err := newService(c, &fakeTS{st: down}, pt, nil).Discover(context.Background()); !errors.Is(err, ErrTailscaleNotRunning) {
		t.Errorf("NeedsLogin: %v", err)
	}
	if _, err := newService(c, nil, pt, nil).Discover(context.Background()); !errors.Is(err, tailnet.ErrNotInstalled) {
		t.Errorf("no CLI: %v", err)
	}
	boom := errors.New("tailscaled not responding")
	if _, err := newService(c, &fakeTS{err: boom}, pt, nil).Discover(context.Background()); !errors.Is(err, boom) {
		t.Errorf("CLI error: %v", err)
	}
	if len(pt.calls) != 0 {
		t.Errorf("probes ran without a running tailnet: %v", pt.calls)
	}
}

func TestPairCreatesDevice(t *testing.T) {
	st, c := newFakeST(t)
	s := newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), nil)
	cand := model.Candidate{NodeName: "laptop", IP: netip.MustParseAddr("100.64.0.2"), DeviceID: idLaptop, Status: StatusReady}
	if err := s.Pair(context.Background(), cand); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 || w[0] != "PUT /rest/config/devices/"+idLaptop {
		t.Fatalf("writes = %v", w)
	}
	want := map[string]any{
		"deviceID":          idLaptop,
		"name":              "laptop",
		"addresses":         []any{"tcp://100.64.0.2:22000", "quic://100.64.0.2:22000"},
		"autoAcceptFolders": false,
		"introducer":        false,
		"compression":       "metadata",
	}
	if got := st.lastBody(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT body = %v\nwant %v", got, want)
	}
	// Pairing again (both users clicking) is an upsert with nothing to change.
	if err := s.Pair(context.Background(), cand); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 {
		t.Fatalf("second Pair wrote again: %v", w)
	}
	st.mu.Lock()
	n := 0
	for _, d := range st.devices {
		if d["deviceID"] == idLaptop {
			n++
		}
	}
	st.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d device entries for the laptop", n)
	}
}

func TestPairMergesExistingDevice(t *testing.T) {
	st, c := newFakeST(t)
	st.addDevice(map[string]any{
		"deviceID": idLaptop, "name": "My Laptop", "addresses": []any{"dynamic"},
		"autoAcceptFolders": true, "maxSendKbps": 100.0,
	})
	s := newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), nil)
	err := s.Pair(context.Background(), model.Candidate{NodeName: "laptop", IP: netip.MustParseAddr("100.64.0.2"), DeviceID: idLaptop})
	if err != nil {
		t.Fatal(err)
	}
	got := st.lastBody()
	if got["name"] != "My Laptop" || got["autoAcceptFolders"] != true || got["maxSendKbps"] != 100.0 {
		t.Errorf("existing settings not kept: %v", got)
	}
	if !reflect.DeepEqual(got["addresses"], []any{"dynamic", "tcp://100.64.0.2:22000", "quic://100.64.0.2:22000"}) {
		t.Errorf("addresses = %v", got["addresses"])
	}
}

func TestPairRejects(t *testing.T) {
	st, c := newFakeST(t)
	s := newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), nil)
	ctx := context.Background()
	cases := []struct {
		name string
		c    model.Candidate
		want error
	}{
		{"lan address", model.Candidate{IP: netip.MustParseAddr("192.168.1.20"), DeviceID: idLaptop}, ErrNotTailnetAddress},
		{"no address", model.Candidate{DeviceID: idLaptop}, ErrNotTailnetAddress},
		{"self status", model.Candidate{IP: netip.MustParseAddr("100.64.0.10"), DeviceID: idSelf, Status: StatusSelf}, ErrSelf},
		{"own id", model.Candidate{IP: netip.MustParseAddr("100.64.0.10"), DeviceID: idSelf}, ErrSelf},
		{"bad id", model.Candidate{IP: netip.MustParseAddr("100.64.0.2"), DeviceID: "not-an-id"}, deviceid.ErrInvalid},
	}
	for _, tc := range cases {
		if err := s.Pair(ctx, tc.c); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if w := st.writeLog(); len(w) != 0 {
		t.Fatalf("rejected pairs wrote: %v", w)
	}
}

func TestAccept(t *testing.T) {
	st, c := newFakeST(t)
	s := newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), nil)
	ctx := context.Background()
	pd := model.PendingDevice{
		DeviceID: idFriend, Name: "friend-syncthing-name",
		Addr: netip.MustParseAddrPort("100.64.0.8:51234"),
	}
	if err := s.Accept(ctx, pd); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("unverified Accept: %v", err)
	}
	if w := st.writeLog(); len(w) != 0 {
		t.Fatalf("unverified Accept wrote: %v", w)
	}
	pd.Verified = true
	pd.Node = &model.Candidate{NodeName: "friend-pc", IP: netip.MustParseAddr("100.64.0.8")}
	if err := s.Accept(ctx, pd); err != nil {
		t.Fatal(err)
	}
	got := st.lastBody()
	if got["name"] != "friend-pc" || got["autoAcceptFolders"] != false ||
		!reflect.DeepEqual(got["addresses"], []any{"tcp://100.64.0.8:22000", "quic://100.64.0.8:22000"}) {
		t.Fatalf("PUT body = %v", got)
	}
	// Without a node, the pending address's IP (never its source port) is used.
	pd2 := model.PendingDevice{DeviceID: idOther, Name: "other", Addr: netip.MustParseAddrPort("100.64.0.3:40000"), Verified: true}
	if err := s.Accept(ctx, pd2); err != nil {
		t.Fatal(err)
	}
	if got := st.lastBody(); !reflect.DeepEqual(got["addresses"], []any{"tcp://100.64.0.3:22000", "quic://100.64.0.3:22000"}) {
		t.Fatalf("addresses = %v", got["addresses"])
	}
}

func TestDecline(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idFriend] = pendingDeviceJSON{Name: "friend", Address: "100.64.0.8:5000"}
	ps, err := prefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), ps)
	if err := s.Decline(context.Background(), idFriend); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 || w[0] != "DELETE /rest/cluster/pending/devices?device="+idFriend {
		t.Fatalf("writes = %v", w)
	}
	if len(st.pendingDevices) != 0 {
		t.Error("pending device not removed")
	}
	if !ps.Get().DeviceDismissed(idFriend) {
		t.Error("declined device not recorded in dismissedDevices")
	}
	if err := s.Decline(context.Background(), "garbage"); !errors.Is(err, deviceid.ErrInvalid) {
		t.Errorf("bad ID: %v", err)
	}
}

func TestWiden(t *testing.T) {
	st, c := newFakeST(t)
	st.addDevice(map[string]any{"deviceID": idLaptop, "addresses": []any{"tcp://100.64.0.2:22000", "quic://100.64.0.2:22000"}})
	ps, err := prefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), ps)
	ctx := context.Background()
	if err := s.Widen(ctx, idLaptop); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 || w[0] != "PATCH /rest/config/devices/"+idLaptop {
		t.Fatalf("writes = %v", w)
	}
	want := []any{"tcp://100.64.0.2:22000", "quic://100.64.0.2:22000", "dynamic"}
	st.mu.Lock()
	got := st.device(idLaptop)["addresses"]
	st.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	if !ps.Get().IsWidened(idLaptop) {
		t.Fatal("widened[id] not recorded")
	}
	if err := s.Widen(ctx, idLaptop); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 {
		t.Fatalf("second Widen wrote: %v", w)
	}

	// Without prefs, an address list that already has "dynamic" is left alone.
	s.Prefs = nil
	if err := s.Widen(ctx, idLaptop); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 {
		t.Fatalf("dynamic appended twice: %v", w)
	}
	if err := s.Widen(ctx, idFriend); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unknown device: %v", err)
	}
}

func TestCandidateStatus(t *testing.T) {
	conf := map[string]bool{idMac: true}
	cases := []struct {
		id   string
		err  error
		want string
	}{
		{idLaptop, nil, StatusReady},
		{idMac, nil, StatusPaired},
		{idSelf, nil, StatusSelf},
		{"", ErrRefused, StatusBlocked},
		{"", ErrTimeout, StatusNoSyncthing},
		{"", ErrNoSyncthing, StatusNoSyncthing},
		{"", context.DeadlineExceeded, StatusNoSyncthing},
	}
	for _, tc := range cases {
		if got := candidateStatus(tc.id, tc.err, idSelf, conf); got != tc.want {
			t.Errorf("candidateStatus(%q, %v) = %q, want %q", deviceid.Short(tc.id), tc.err, got, tc.want)
		}
	}
}
