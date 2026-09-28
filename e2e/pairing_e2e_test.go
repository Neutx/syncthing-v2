//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/pairing"
)

// TestPairing drives the full pairing flow of spec §10.3 between two real
// Syncthing instances: A on 127.0.0.2 and B on 127.0.0.3. The fake tailnet
// maps both addresses to nodes of the same owner and 127.0.0.4 to another
// owner.
func TestPairing(t *testing.T) {
	preflight(t)
	bin := syncthingBinary(t)
	root := t.TempDir()
	a := startInstance(t, bin, root, "e2e-a", ipA, pickGUIPort(t, guiPortA))
	b := startInstance(t, bin, root, "e2e-b", ipB, pickGUIPort(t, guiPortB))

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	const alice, bob = "alice@example.com", "bob@example.org"
	nodeA := node(a.name, ipA, 1, alice)
	nodeB := node(b.name, ipB, 1, alice)
	nodeOther := node("e2e-other", ipOther, 2, bob)
	viewA := view(nodeA, nodeB, nodeOther)
	viewB := view(nodeB, nodeA, nodeOther)

	probe := sandboxProbe(a.syncAddr(), b.syncAddr())
	svcA := &pairing.Service{C: a.client, TS: fakeTailnet{viewA}, Policy: loopback, Probe: probe}
	svcB := &pairing.Service{C: b.client, TS: fakeTailnet{viewB}, Policy: loopback, Probe: probe}

	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	var candA, candB model.Candidate // B as seen from A, and A as seen from B
	var verified model.PendingDevice // A's request on B, from B's Watcher

	// 4.1: Probe returns each instance's `syncthing device-id`.
	step("probe returns device IDs", func(t *testing.T) {
		for _, in := range []*instance{a, b} {
			got, err := pairing.Probe(ctx, in.syncAddr())
			if err != nil {
				t.Fatalf("Probe(%s): %v", in.syncAddr(), err)
			}
			if got != in.id {
				t.Errorf("Probe(%s) = %s, `syncthing device-id` printed %s", in.syncAddr(), got, in.id)
			}
			if c := in.certID(t); c != in.id {
				t.Errorf("deviceid.FromCert(%s cert.pem) = %s, `syncthing device-id` printed %s", in.name, c, in.id)
			}
		}

		cands, err := svcA.Discover(ctx)
		if err != nil {
			t.Fatalf("Discover on A: %v", err)
		}
		if len(cands) != 2 {
			t.Fatalf("Discover on A returned %d candidates, want 2: %+v", len(cands), cands)
		}
		candB = cands[0] // same owner first
		if candB.NodeName != b.name || candB.IP != ipB || !candB.SameOwner || candB.DeviceID != b.id || candB.Status != pairing.StatusReady {
			t.Errorf("A's candidate for B = %+v, want %s at %s, same owner, ID %s, status %s", candB, b.name, ipB, b.id, pairing.StatusReady)
		}
		other := cands[1]
		if other.NodeName != "e2e-other" || other.SameOwner || other.LoginName != bob || other.Status != pairing.StatusBlocked || other.DeviceID != "" {
			t.Errorf("A's other-owner candidate = %+v, want e2e-other owned by %s, blocked, no ID", other, bob)
		}

		cands, err = svcB.Discover(ctx)
		if err != nil {
			t.Fatalf("Discover on B: %v", err)
		}
		if len(cands) == 0 {
			t.Fatal("Discover on B returned no candidates")
		}
		candA = cands[0]
		if candA.NodeName != a.name || candA.DeviceID != a.id || candA.Status != pairing.StatusReady || !candA.SameOwner {
			t.Errorf("B's candidate for A = %+v, want %s, ID %s, ready, same owner", candA, a.name, a.id)
		}
	})

	// 4.2: after the probes, /rest/cluster/pending/devices is empty on both.
	step("probes leave no pending devices", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			for _, in := range []*instance{a, b} {
				if _, err := pairing.Probe(ctx, in.syncAddr()); err != nil {
					t.Fatalf("Probe(%s): %v", in.syncAddr(), err)
				}
			}
		}
		// A completed handshake would be recorded at once; give Syncthing
		// time to process anything it received before looking.
		time.Sleep(3 * time.Second)
		for _, in := range []*instance{a, b} {
			p, err := in.pendingDevices(ctx)
			if err != nil {
				t.Fatalf("%s: pending devices: %v", in.name, err)
			}
			if len(p) != 0 {
				t.Errorf("%s: %d pending devices after probing, want none: %v", in.name, len(p), p)
			}
		}
	})

	// 4.3: A Pairs B, so B has a pending A, and Decide returns prompt with Verified.
	// 4.8: the same pending request from an address whose probe returns another ID is ignored.
	step("pair gives a verified prompt on B", func(t *testing.T) {
		if err := svcA.Pair(ctx, candB); err != nil {
			t.Fatalf("Pair(B) on A: %v", err)
		}
		dev, err := a.device(ctx, b.id)
		if err != nil {
			t.Fatalf("A: device B after Pair: %v", err)
		}
		wantAddrs := []string{"tcp://127.0.0.3:22000", "quic://127.0.0.3:22000"}
		if !slices.Equal(dev.Addresses, wantAddrs) || dev.Name != b.name || dev.AutoAcceptFolders || dev.Introducer || dev.Compression != "metadata" {
			t.Errorf("A's config for B = %+v, want name %s, addresses %v, no auto-accept, no introducer, compression metadata", dev, b.name, wantAddrs)
		}

		var raw pendingDevice
		waitFor(t, "B records A as a pending device", func() (bool, error) {
			p, err := b.pendingDevices(ctx)
			raw = p[a.id]
			return raw.Address != "", err
		})
		from, err := netip.ParseAddrPort(strings.TrimPrefix(strings.TrimPrefix(raw.Address, "tcp://"), "quic://"))
		if err != nil || from.Addr() != ipA {
			t.Fatalf("B's pending A comes from %q, want %s (Syncthing dials from its listen address)", raw.Address, ipA)
		}
		pd := model.PendingDevice{DeviceID: a.id, Name: raw.Name, Addr: from}

		// Decide with a real probe of the pending address.
		probed, perr := pairing.Probe(ctx, netip.AddrPortFrom(ipA, pairing.SyncPort))
		d := loopback.Decide(pd, viewB, probed, perr)
		if d.Action != pairing.ActionPrompt || d.Reason != pairing.ReasonSameOwner || d.Node == nil || d.Node.HostName != a.name {
			t.Errorf("Decide(A on B) = %+v (node %+v), want prompt, %q, node %s", d, d.Node, pairing.ReasonSameOwner, a.name)
		}
		// The same request while the tailnet says 127.0.0.2 belongs to another owner.
		otherA := node(a.name, ipA, 2, bob)
		if d := loopback.Decide(pd, view(nodeB, otherA), probed, perr); d.Action != pairing.ActionPrompt || d.Reason != pairing.ReasonOtherOwner {
			t.Errorf("Decide(A on B, other owner) = %+v, want prompt, %q", d, pairing.ReasonOtherOwner)
		}

		// 4.8: pending ID A, but the device answering on the pending address is B.
		// Evaluated by a third computer, since B's own address is "this computer" to B.
		wrong := pd
		wrong.Addr = b.syncAddr()
		probedB, perrB := pairing.Probe(ctx, b.syncAddr())
		if d := loopback.Decide(wrong, view(nodeOther, nodeA, nodeB), probedB, perrB); d.Action != pairing.ActionIgnore || d.Reason != pairing.ReasonMismatch {
			t.Errorf("Decide(A's ID from B's address) = %+v, want ignore, %q", d, pairing.ReasonMismatch)
		}
		// On B itself, a request from B's own address is ignored before any probe result counts.
		if d := loopback.Decide(wrong, viewB, probedB, perrB); d.Action != pairing.ActionIgnore || d.Reason != pairing.ReasonSelf {
			t.Errorf("Decide(A's ID from B's address, on B) = %+v, want ignore, %q", d, pairing.ReasonSelf)
		}
		// The same case through a Watcher: an address-rewriting middlebox makes
		// A's address lead to B's Syncthing.
		snat := &pairing.Service{C: b.client, TS: fakeTailnet{viewB}, Policy: loopback,
			Probe: func(ctx context.Context, ap netip.AddrPort) (string, error) {
				if ap.Addr() == ipA {
					ap = b.syncAddr()
				}
				return pairing.Probe(ctx, ap)
			}}
		var ignored []model.PendingDevice
		ws := pairing.NewWatcher(snat, nil)
		ws.OnIgnore = func(p model.PendingDevice) { ignored = append(ignored, p) }
		ws.OnPrompt = func(p model.PendingDevice) { t.Errorf("mismatched request produced a prompt: %+v", p) }
		if err := ws.Check(ctx); err != nil {
			t.Fatalf("mismatch Watcher.Check: %v", err)
		}
		if len(ignored) != 1 || ignored[0].DeviceID != a.id || ignored[0].Reason != pairing.ReasonMismatch || len(ws.Pending()) != 0 {
			t.Errorf("mismatch Watcher: ignored %+v, pending %+v; want A ignored for %q and no prompt", ignored, ws.Pending(), pairing.ReasonMismatch)
		}

		// The real Watcher on B turns the request into a verified prompt.
		var prompts []model.PendingDevice
		w := pairing.NewWatcher(svcB, nil)
		w.OnPrompt = func(p model.PendingDevice) { prompts = append(prompts, p) }
		w.OnIgnore = func(p model.PendingDevice) { t.Errorf("B's Watcher ignored %s: %s", p.DeviceID, p.Reason) }
		if err := w.Check(ctx); err != nil {
			t.Fatalf("Watcher.Check on B: %v", err)
		}
		if len(prompts) != 1 {
			t.Fatalf("B's Watcher produced %d prompts, want 1: %+v", len(prompts), prompts)
		}
		verified = prompts[0]
		if !verified.Verified || verified.DeviceID != a.id || verified.Addr.Addr() != ipA || verified.Reason != pairing.ReasonSameOwner ||
			verified.Node == nil || verified.Node.NodeName != a.name || !verified.Node.SameOwner || verified.Node.DeviceID != a.id {
			t.Errorf("B's prompt = %+v (node %+v), want a verified same-owner request from %s at %s", verified, verified.Node, a.name, ipA)
		}
	})

	// 4.4: Accept on B brings both connected within 60 s.
	step("accept connects both", func(t *testing.T) {
		unverified := verified
		unverified.Verified = false
		if err := svcB.Accept(ctx, unverified); !errors.Is(err, pairing.ErrNotVerified) {
			t.Fatalf("Accept(unverified) = %v, want ErrNotVerified", err)
		}
		if err := svcB.Accept(ctx, verified); err != nil {
			t.Fatalf("Accept(A) on B: %v", err)
		}
		dev, err := b.device(ctx, a.id)
		if err != nil {
			t.Fatalf("B: device A after Accept: %v", err)
		}
		wantAddrs := []string{"tcp://127.0.0.2:22000", "quic://127.0.0.2:22000"}
		if !slices.Equal(dev.Addresses, wantAddrs) || dev.AutoAcceptFolders || dev.Name != a.name {
			t.Errorf("B's config for A = %+v, want name %s, addresses %v, no auto-accept", dev, a.name, wantAddrs)
		}
		waitConnected(ctx, t, a, b)
		waitNoPending(ctx, t, a, b)
		w := pairing.NewWatcher(svcB, nil)
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if p := w.Pending(); len(p) != 0 {
			t.Errorf("B's Watcher still prompts after Accept: %+v", p)
		}
	})

	// 4.5: ShareFolder on A leads to a pending folder on B; AcceptFolder into
	// SafeFolderDir; files written on either side appear on the other within 60 s.
	step("shared folder syncs both ways", func(t *testing.T) {
		folderID, err := pairing.NewFolderID()
		if err != nil {
			t.Fatal(err)
		}
		const label = "E2E Share"
		pathA := filepath.Join(a.dir, "Sync", label)
		for i := 0; i < 2; i++ { // the second call must be a no-op
			if err := svcA.ShareFolder(ctx, folderID, label, pathA, []string{b.id}); err != nil {
				t.Fatalf("ShareFolder on A (call %d): %v", i+1, err)
			}
		}
		fa, err := a.folder(ctx, folderID)
		if err != nil {
			t.Fatalf("A: folder after ShareFolder: %v", err)
		}
		if got := folderDevices(fa); !slices.Equal(got, sorted(a.id, b.id)) || fa.Path != pathA || fa.Label != label || fa.Type != "sendreceive" {
			t.Errorf("A's folder = %+v (devices %v), want %s at %s shared with exactly A and B", fa, got, label, pathA)
		}

		w := pairing.NewWatcher(svcB, nil)
		var offer model.PendingFolder
		waitFor(t, "B sees A's folder offer", func() (bool, error) {
			if err := w.Check(ctx); err != nil {
				return false, err
			}
			for _, f := range w.PendingFolders() {
				if f.FolderID == folderID && f.FromDevice == a.id {
					offer = f
					return true, nil
				}
			}
			return false, nil
		})
		if offer.Label != label {
			t.Errorf("B's folder offer label = %q, want %q", offer.Label, label)
		}

		baseB := filepath.Join(b.dir, "Sync")
		dest, err := pairing.SafeFolderDir(baseB, offer.Label, offer.FolderID)
		if err != nil {
			t.Fatalf("SafeFolderDir: %v", err)
		}
		if dest != filepath.Join(baseB, label) {
			t.Errorf("SafeFolderDir = %s, want %s", dest, filepath.Join(baseB, label))
		}
		if err := svcB.AcceptFolder(ctx, offer, dest); err != nil {
			t.Fatalf("AcceptFolder on B: %v", err)
		}
		fb, err := b.folder(ctx, folderID)
		if err != nil {
			t.Fatalf("B: folder after AcceptFolder: %v", err)
		}
		if got := folderDevices(fb); !slices.Equal(got, sorted(a.id, b.id)) || fb.Path != dest || fb.Type != "sendreceive" {
			t.Errorf("B's folder = %+v (devices %v), want sendreceive at %s shared with A and B", fb, got, dest)
		}

		syncFile(ctx, t, a, pathA, b, dest, folderID, "from-a.txt")
		syncFile(ctx, t, b, dest, a, pathA, folderID, "from-b.txt")
	})

	// 4.6: Widen adds "dynamic" (once).
	step("widen adds dynamic", func(t *testing.T) {
		want := []string{"tcp://127.0.0.3:22000", "quic://127.0.0.3:22000", pairing.DynamicAddress}
		for i := 0; i < 2; i++ {
			if err := svcA.Widen(ctx, b.id); err != nil {
				t.Fatalf("Widen(B) on A (call %d): %v", i+1, err)
			}
			dev, err := a.device(ctx, b.id)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(dev.Addresses, want) {
				t.Errorf("A's addresses for B after Widen call %d = %v, want %v", i+1, dev.Addresses, want)
			}
		}
	})

	// 4.7: pairing on both sides at the same time creates no duplicates.
	step("simultaneous pairing creates no duplicates", func(t *testing.T) {
		if err := a.client.Send(ctx, http.MethodDelete, "/rest/config/devices/"+url.PathEscape(b.id), nil); err != nil {
			t.Fatalf("A: remove B: %v", err)
		}
		if err := b.client.Send(ctx, http.MethodDelete, "/rest/config/devices/"+url.PathEscape(a.id), nil); err != nil {
			t.Fatalf("B: remove A: %v", err)
		}
		waitFor(t, "A and B disconnected after removal", func() (bool, error) {
			ca, err := a.connected(ctx, b.id)
			if err != nil {
				return false, err
			}
			cb, err := b.connected(ctx, a.id)
			return !ca && !cb, err
		})
		// A dial that raced the removal may have left a pending entry.
		if err := svcA.Decline(ctx, b.id); err != nil {
			t.Fatalf("A: clear pending B: %v", err)
		}
		if err := svcB.Decline(ctx, a.id); err != nil {
			t.Fatalf("B: clear pending A: %v", err)
		}

		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for i := 0; i < 3; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); errs <- svcA.Pair(ctx, candB) }()
			go func() { defer wg.Done(); errs <- svcB.Pair(ctx, candA) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("simultaneous Pair: %v", err)
			}
		}

		waitConnected(ctx, t, a, b)
		for _, c := range []struct {
			in      *instance
			peer    *instance
			wantTCP string
		}{{a, b, "tcp://127.0.0.3:22000"}, {b, a, "tcp://127.0.0.2:22000"}} {
			devs, err := c.in.devices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := map[string]int{}
			for _, d := range devs {
				count[d.DeviceID]++
			}
			if len(devs) != 2 || count[c.in.id] != 1 || count[c.peer.id] != 1 {
				t.Errorf("%s: configured devices %v, want exactly itself and %s once each", c.in.name, count, c.peer.name)
			}
			dev, err := c.in.device(ctx, c.peer.id)
			if err != nil {
				t.Fatal(err)
			}
			wantQUIC := strings.Replace(c.wantTCP, "tcp://", "quic://", 1)
			if !slices.Equal(dev.Addresses, []string{c.wantTCP, wantQUIC}) {
				t.Errorf("%s: addresses for %s = %v, want [%s %s] with no duplicates", c.in.name, c.peer.name, dev.Addresses, c.wantTCP, wantQUIC)
			}
		}
		waitNoPending(ctx, t, a, b)
		for _, svc := range []*pairing.Service{svcA, svcB} {
			w := pairing.NewWatcher(svc, nil)
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if p := w.Pending(); len(p) != 0 {
				t.Errorf("a prompt appeared after simultaneous pairing: %+v", p)
			}
		}
	})
}

// waitConnected waits until x and y report each other as connected.
func waitConnected(ctx context.Context, t *testing.T, x, y *instance) {
	t.Helper()
	waitFor(t, x.name+" and "+y.name+" connected", func() (bool, error) {
		cx, err := x.connected(ctx, y.id)
		if err != nil || !cx {
			return false, err
		}
		return y.connected(ctx, x.id)
	})
}

// waitNoPending waits until no instance lists a pending device.
func waitNoPending(ctx context.Context, t *testing.T, ins ...*instance) {
	t.Helper()
	for _, in := range ins {
		waitFor(t, in.name+" has no pending devices", func() (bool, error) {
			p, err := in.pendingDevices(ctx)
			return len(p) == 0, err
		})
	}
}

// syncFile writes name into fromDir, rescans the folder on from and waits
// until to has the same content in toDir.
func syncFile(ctx context.Context, t *testing.T, from *instance, fromDir string, to *instance, toDir, folderID, name string) {
	t.Helper()
	content := []byte(from.name + " " + randomText(t) + "\n")
	if err := os.WriteFile(filepath.Join(fromDir, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := from.scan(ctx, folderID); err != nil {
		t.Fatalf("%s: rescan: %v", from.name, err)
	}
	waitFor(t, name+" from "+from.name+" appears on "+to.name, func() (bool, error) {
		got, err := os.ReadFile(filepath.Join(toDir, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return bytes.Equal(got, content), err
	})
}

func folderDevices(f folderConfig) []string {
	out := make([]string, 0, len(f.Devices))
	for _, d := range f.Devices {
		out = append(out, d.DeviceID)
	}
	slices.Sort(out)
	return out
}

func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}
