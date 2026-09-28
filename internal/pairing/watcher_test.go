package pairing

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

func deviceidFor(name string) string { return deviceid.FromCert([]byte("synthetic " + name)) }

// recorder collects Watcher callbacks.
type recorder struct {
	mu      sync.Mutex
	prompts []model.PendingDevice
	ignores []model.PendingDevice
	folders []model.PendingFolder
	changes int
	signal  chan struct{}
}

func newRecorder(w *Watcher) *recorder {
	r := &recorder{signal: make(chan struct{}, 64)}
	w.OnPrompt = func(p model.PendingDevice) {
		r.mu.Lock()
		r.prompts = append(r.prompts, p)
		r.mu.Unlock()
		r.signal <- struct{}{}
	}
	w.OnIgnore = func(p model.PendingDevice) {
		r.mu.Lock()
		r.ignores = append(r.ignores, p)
		r.mu.Unlock()
	}
	w.OnFolder = func(f model.PendingFolder) {
		r.mu.Lock()
		r.folders = append(r.folders, f)
		r.mu.Unlock()
	}
	w.OnChange = func() {
		r.mu.Lock()
		r.changes++
		r.mu.Unlock()
	}
	return r
}

func (r *recorder) snapshot() (prompts, ignores []model.PendingDevice, folders []model.PendingFolder, changes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.PendingDevice(nil), r.prompts...), append([]model.PendingDevice(nil), r.ignores...),
		append([]model.PendingFolder(nil), r.folders...), r.changes
}

func ids(ps []model.PendingDevice) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		out[p.DeviceID] = p.Reason
	}
	return out
}

func TestWatcherEvaluatesAndDedupes(t *testing.T) {
	st, c := newFakeST(t)
	idDismissed := deviceidFor("dismissed")
	idConfigured := deviceidFor("configured")
	st.addDevice(map[string]any{"deviceID": idConfigured, "name": "configured"})
	st.pendingDevices = map[string]pendingDeviceJSON{
		idLaptop:     {Name: "laptop", Address: "100.64.0.2:50001"},
		idFriend:     {Name: "friend", Address: "tcp://100.64.0.8:50002"},
		idLAN:        {Name: "lan", Address: "192.168.1.50:22000"},
		idOther:      {Name: "spoof", Address: "100.64.0.3:50003"},
		idMac:        {Name: "ghost", Address: "100.64.0.99:50004"},
		idDismissed:  {Name: "dismissed", Address: "100.64.0.2:50005"},
		idConfigured: {Name: "configured", Address: "100.64.0.2:50006"},
		"not-an-id":  {Name: "garbage", Address: "100.64.0.2:50007"},
	}
	pt := newProbeTable(map[string]probeResult{
		"100.64.0.2": {id: idLaptop},
		"100.64.0.8": {id: idFriend},
		"100.64.0.3": {id: idMac}, // the node at .3 is really the mac: identity mismatch for idOther
	})
	ps, err := prefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.DismissDevice(idDismissed); err != nil {
		t.Fatal(err)
	}
	ts := &fakeTS{st: testTailnet()}
	w := NewWatcher(newService(c, ts, pt, ps), func(id string) bool { return ps.Get().DeviceDismissed(id) })
	rec := newRecorder(w)
	ctx := context.Background()

	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	prompts, ignores, _, changes := rec.snapshot()
	if got, want := ids(prompts), map[string]string{idLaptop: ReasonSameOwner, idFriend: ReasonOtherOwner}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts = %v, want %v", got, want)
	}
	if got, want := ids(ignores), map[string]string{idLAN: ReasonNotTailnet, idOther: ReasonMismatch, idMac: ReasonUnknownNode}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ignores = %v, want %v", got, want)
	}
	if changes != 1 {
		t.Errorf("OnChange called %d times, want 1", changes)
	}
	for _, p := range prompts {
		if !p.Verified || p.Node == nil || p.Node.DeviceID != p.DeviceID {
			t.Errorf("prompt %+v: want Verified with Node", p)
		}
		switch p.DeviceID {
		case idLaptop:
			if !p.Node.SameOwner || p.Node.NodeName != "laptop" {
				t.Errorf("laptop node = %+v", p.Node)
			}
		case idFriend:
			if p.Node.SameOwner || p.Node.LoginName != "friend@example.org" {
				t.Errorf("friend node = %+v", p.Node)
			}
		}
	}
	if got := w.Pending(); len(got) != 2 || !got[0].Verified || !got[1].Verified {
		t.Fatalf("Pending() = %+v", got)
	}
	// LAN requests never reach Tailscale or the prober.
	if n := pt.count("192.168.1.50"); n != 0 {
		t.Errorf("LAN address probed %d times", n)
	}
	// Unknown nodes are not probed either.
	if n := pt.count("100.64.0.99"); n != 0 {
		t.Errorf("unknown node probed %d times", n)
	}
	probesAfterFirst := pt.count("100.64.0.2")
	if probesAfterFirst != 1 {
		t.Errorf("laptop IP probed %d times, want 1 (dismissed/configured/garbage IDs are skipped)", probesAfterFirst)
	}

	// Second check, same pending set with new source ports (Syncthing redials):
	// no new callbacks and no new probes.
	st.mu.Lock()
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:59999"}
	st.mu.Unlock()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	prompts2, ignores2, _, changes2 := rec.snapshot()
	if len(prompts2) != 2 || len(ignores2) != 3 || changes2 != 1 {
		t.Fatalf("second check produced callbacks: %d prompts, %d ignores, %d changes", len(prompts2), len(ignores2), changes2)
	}
	if n := pt.count("100.64.0.2"); n != probesAfterFirst {
		t.Errorf("re-probed an already decided device (%d probes)", n)
	}

	// The laptop gets configured (both sides clicked Pair): its prompt is dropped.
	st.addDevice(map[string]any{"deviceID": idLaptop})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.Pending(); len(got) != 1 || got[0].DeviceID != idFriend {
		t.Fatalf("after configure: Pending() = %+v", got)
	}
	// The friend's request disappears (declined elsewhere): dropped too.
	st.mu.Lock()
	delete(st.pendingDevices, idFriend)
	st.mu.Unlock()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.Pending(); len(got) != 0 {
		t.Fatalf("after removal: Pending() = %+v", got)
	}
	if _, _, _, ch := rec.snapshot(); ch != 3 {
		t.Errorf("OnChange count = %d, want 3", ch)
	}

	// A request that comes back after it left is evaluated again.
	st.mu.Lock()
	st.pendingDevices[idFriend] = pendingDeviceJSON{Name: "friend", Address: "100.64.0.8:50010"}
	st.mu.Unlock()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _, _, _ := rec.snapshot(); len(p) != 3 || p[2].DeviceID != idFriend {
		t.Fatalf("returning request not prompted again: %d prompts", len(p))
	}
}

func TestWatcherDismissAfterPrompt(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
	pt := newProbeTable(map[string]probeResult{"100.64.0.2": {id: idLaptop}})
	dismissed := map[string]bool{}
	var mu sync.Mutex
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, pt, nil), func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		return dismissed[id]
	})
	newRecorder(w)
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.Pending()) != 1 {
		t.Fatal("no prompt")
	}
	mu.Lock()
	dismissed[idLaptop] = true
	mu.Unlock()
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.Pending()) != 0 {
		t.Fatal("dismissed device still prompted")
	}
}

func TestWatcherResolve(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, newProbeTable(map[string]probeResult{"100.64.0.2": {id: idLaptop}}), nil), nil)
	rec := newRecorder(w)
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Resolve(idLaptop)
	if len(w.Pending()) != 0 {
		t.Fatal("Resolve did not drop the prompt")
	}
	if _, _, _, ch := rec.snapshot(); ch != 2 {
		t.Fatalf("OnChange count = %d, want 2", ch)
	}
	w.Resolve(idLaptop) // no prompt left: no change
	if _, _, _, ch := rec.snapshot(); ch != 2 {
		t.Fatalf("OnChange count = %d after a no-op Resolve", ch)
	}
}

func TestWatcherProbeFailureRetry(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
	pt := newProbeTable(map[string]probeResult{"100.64.0.2": {err: ErrTimeout}})
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, pt, nil), nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	rec := newRecorder(w)
	ctx := context.Background()

	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ig, _, _ := rec.snapshot(); len(ig) != 1 || ig[0].Reason != ReasonProbeFailed {
		t.Fatalf("ignores = %+v", ig)
	}
	now = now.Add(retryAfter - time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if n := pt.count("100.64.0.2"); n != 1 {
		t.Fatalf("re-probed before retryAfter (%d probes)", n)
	}

	// After retryAfter the probe is repeated; this time it succeeds.
	pt.mu.Lock()
	pt.results["100.64.0.2"] = probeResult{id: idLaptop}
	pt.mu.Unlock()
	now = now.Add(2 * time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	p, ig, _, _ := rec.snapshot()
	if len(p) != 1 || !p[0].Verified || len(ig) != 1 {
		t.Fatalf("after retry: %d prompts, %d ignores (the failure is logged once)", len(p), len(ig))
	}
}

func TestWatcherTailscaleUnavailable(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
	pt := newProbeTable(map[string]probeResult{"100.64.0.2": {id: idLaptop}})
	ts := &fakeTS{st: testTailnet(), err: errors.New("tailscaled unavailable")}
	w := NewWatcher(newService(c, ts, pt, nil), nil)
	rec := newRecorder(w)
	if err := w.Check(context.Background()); err == nil {
		t.Fatal("expected the Tailscale error")
	}
	if p, ig, _, _ := rec.snapshot(); len(p) != 0 || len(ig) != 0 {
		t.Fatalf("decided without Tailscale: %v %v", p, ig)
	}
	// Tailscale comes back: the request is evaluated on the next check.
	ts.err = nil
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, _, _, _ := rec.snapshot(); len(p) != 1 {
		t.Fatalf("not retried after Tailscale recovered: %d prompts", len(p))
	}
	if n := ts.calls.Load(); n != 2 {
		t.Errorf("tailscale status called %d times, want 2 (once per check)", n)
	}
}

func TestWatcherLANOnlyDoesNotAskTailscale(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLAN] = pendingDeviceJSON{Name: "lan", Address: "10.0.0.5:22000"}
	ts := &fakeTS{st: testTailnet()}
	w := NewWatcher(newService(c, ts, newProbeTable(nil), nil), nil)
	rec := newRecorder(w)
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := ts.calls.Load(); n != 0 {
		t.Fatalf("tailscale status called %d times for a LAN-only request", n)
	}
	if _, ig, _, _ := rec.snapshot(); len(ig) != 1 || ig[0].Reason != ReasonNotTailnet {
		t.Fatalf("ignores = %+v", ig)
	}
}

func TestWatcherFolders(t *testing.T) {
	st, c := newFakeST(t)
	st.addDevice(map[string]any{"deviceID": idLaptop, "name": "laptop"})
	st.pendingFolders["offer-0001"] = map[string]string{idLaptop: "Photos", idFriend: "Photos"}
	st.pendingFolders["offer-0002"] = map[string]string{idFriend: "Secret"}
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, newProbeTable(nil), nil), nil)
	rec := newRecorder(w)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := []model.PendingFolder{{FolderID: "offer-0001", Label: "Photos", FromDevice: idLaptop}}
	if _, _, f, _ := rec.snapshot(); !reflect.DeepEqual(f, want) {
		t.Fatalf("OnFolder calls = %+v, want %+v (once, configured senders only)", f, want)
	}
	if got := w.PendingFolders(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PendingFolders() = %+v", got)
	}
	st.mu.Lock()
	delete(st.pendingFolders, "offer-0001")
	st.mu.Unlock()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.PendingFolders(); len(got) != 0 {
		t.Fatalf("accepted offer still listed: %+v", got)
	}
}

func TestWatcherRunAndKick(t *testing.T) {
	st, c := newFakeST(t)
	pt := newProbeTable(map[string]probeResult{"100.64.0.2": {id: idLaptop}})
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, pt, nil), nil)
	w.Interval = time.Hour // only the initial check and kicks run
	rec := newRecorder(w)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let the initial (empty) check pass, then add a request and kick.
	deadline := time.After(5 * time.Second)
	for {
		if pt.count("100.64.0.2") == 0 {
			st.mu.Lock()
			st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
			st.mu.Unlock()
			w.Kick()
		}
		select {
		case <-rec.signal:
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("Run returned %v", err)
			}
			return
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			cancel()
			t.Fatal("Kick did not trigger a check")
		}
	}
}

func TestParseAddrPort(t *testing.T) {
	cases := map[string]string{
		"100.64.0.2:22000":         "100.64.0.2:22000",
		"tcp://100.64.0.2:22000":   "100.64.0.2:22000",
		"quic://[fd7a::1]:22000":   "[fd7a::1]:22000",
		"[::ffff:100.64.0.2]:1234": "100.64.0.2:1234",
		" 10.0.0.1:1 ":             "10.0.0.1:1",
	}
	for in, want := range cases {
		if got := parseAddrPort(in).String(); got != want {
			t.Errorf("parseAddrPort(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "relay://abc", "100.64.0.2"} {
		if ap := parseAddrPort(bad); ap.IsValid() {
			t.Errorf("parseAddrPort(%q) = %v", bad, ap)
		}
	}
}

func TestWatcherAccept(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
	st.pendingDevices[idLAN] = pendingDeviceJSON{Name: "lan", Address: "192.168.1.50:22000"}
	pt := newProbeTable(map[string]probeResult{"100.64.0.2": {id: idLaptop}})
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, pt, nil), nil)
	rec := newRecorder(w)
	ctx := context.Background()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}

	// IDs without a stored prompt are refused and nothing is written: an ID
	// that failed the policy, a well-formed ID never seen, and garbage.
	for _, id := range []string{idLAN, idFriend, "", "not-a-device-id"} {
		if _, err := w.Accept(ctx, id); !errors.Is(err, ErrNotVerified) {
			t.Fatalf("Accept(%q) = %v, want ErrNotVerified", id, err)
		}
	}
	if wl := st.writeLog(); len(wl) != 0 {
		t.Fatalf("refused accepts wrote: %v", wl)
	}

	pd, err := w.Accept(ctx, idLaptop)
	if err != nil {
		t.Fatal(err)
	}
	if pd.DeviceID != idLaptop || pd.Node == nil || pd.Node.NodeName != "laptop" {
		t.Fatalf("accepted request = %+v", pd)
	}
	got := st.lastBody()
	if got["deviceID"] != idLaptop || got["name"] != "laptop" ||
		!reflect.DeepEqual(got["addresses"], []any{"tcp://100.64.0.2:22000", "quic://100.64.0.2:22000"}) {
		t.Fatalf("PUT body = %v", got)
	}
	if len(w.Pending()) != 0 {
		t.Fatal("prompt not dropped after Accept")
	}
	if _, _, _, ch := rec.snapshot(); ch != 2 {
		t.Fatalf("OnChange count = %d, want 2 (prompt added, prompt resolved)", ch)
	}
	// The prompt is gone, so a second Accept is refused.
	if _, err := w.Accept(ctx, idLaptop); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("second Accept = %v, want ErrNotVerified", err)
	}
}

func TestWatcherAcceptFailureKeepsPrompt(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idLaptop] = pendingDeviceJSON{Name: "laptop", Address: "100.64.0.2:50001"}
	pt := newProbeTable(map[string]probeResult{"100.64.0.2": {id: idLaptop}})
	w := NewWatcher(newService(c, &fakeTS{st: testTailnet()}, pt, nil), nil)
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.myID = idLaptop // Syncthing now reports the requester as itself: upsert refuses
	st.mu.Unlock()
	if _, err := w.Accept(context.Background(), idLaptop); !errors.Is(err, ErrSelf) {
		t.Fatalf("Accept = %v, want ErrSelf", err)
	}
	if len(w.Pending()) != 1 {
		t.Fatal("a failed Accept dropped the prompt")
	}
}

func TestWatcherUnknownNodeRetry(t *testing.T) {
	st, c := newFakeST(t)
	// 100.64.0.77 is inside the tailnet prefix but not in the first status.
	st.pendingDevices[idOther] = pendingDeviceJSON{Name: "new-node", Address: "100.64.0.77:50001"}
	pt := newProbeTable(map[string]probeResult{"100.64.0.77": {id: idOther}})
	ts := &fakeTS{st: testTailnet()}
	w := NewWatcher(newService(c, ts, pt, nil), nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	rec := newRecorder(w)
	ctx := context.Background()

	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ig, _, _ := rec.snapshot(); len(ig) != 1 || ig[0].Reason != ReasonUnknownNode {
		t.Fatalf("ignores = %+v", ig)
	}

	// The node joins the tailnet (our cached status was stale).
	joined := testTailnet()
	joined.Peers = append(joined.Peers, tailnet.Node{
		ID: "n77", HostName: "new-node", OS: "linux", UserID: 1, LoginName: "owner@example.com",
		IPs: []netip.Addr{netip.MustParseAddr("100.64.0.77")}, Online: true,
	})
	ts.st = joined

	now = now.Add(retryAfter - time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("re-evaluated before retryAfter (%d status calls)", n)
	}
	if len(w.Pending()) != 0 {
		t.Fatal("prompted before retryAfter")
	}

	now = now.Add(2 * time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	p, ig, _, _ := rec.snapshot()
	if len(p) != 1 || p[0].DeviceID != idOther || !p[0].Verified || p[0].Reason != ReasonSameOwner {
		t.Fatalf("after retry: prompts = %+v", p)
	}
	if len(ig) != 1 {
		t.Fatalf("the unknown-node verdict was logged %d times, want once", len(ig))
	}
	if n := pt.count("100.64.0.77"); n != 1 {
		t.Fatalf("probes = %d, want 1 (only once the node is known)", n)
	}
}

func TestWatcherUnknownNodeRetryLogsOnce(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingDevices[idOther] = pendingDeviceJSON{Name: "ghost", Address: "100.64.0.77:50001"}
	ts := &fakeTS{st: testTailnet()}
	w := NewWatcher(newService(c, ts, newProbeTable(nil), nil), nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	rec := newRecorder(w)
	for i := 0; i < 3; i++ {
		if err := w.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(retryAfter)
	}
	if n := ts.calls.Load(); n != 3 {
		t.Fatalf("tailscale status calls = %d, want 3 (re-evaluated every retryAfter)", n)
	}
	if p, ig, _, _ := rec.snapshot(); len(p) != 0 || len(ig) != 1 {
		t.Fatalf("still unknown: %d prompts, %d ignores (want 0, 1)", len(p), len(ig))
	}
}
