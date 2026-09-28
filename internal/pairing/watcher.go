package pairing

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

// WatchInterval is the fallback poll interval of the Watcher; the app also
// calls Kick on PendingDevicesChanged and PendingFoldersChanged events.
const WatchInterval = 30 * time.Second

// retryAfter is how long a verdict that may be caused by a transient
// condition is kept before the Watcher evaluates that pending device again:
// "identity not verified" (the probe failed) and "unknown tailnet node" (the
// node may be missing from a stale `tailscale status`). Syncthing keeps
// pending devices across restarts, so these verdicts must not be final.
const retryAfter = 5 * time.Minute

// retryable reports whether a verdict is re-evaluated after retryAfter.
func retryable(reason string) bool {
	return reason == ReasonProbeFailed || reason == ReasonUnknownNode
}

// Watcher turns Syncthing's pending devices and folders into pairing
// prompts. Each pending device is evaluated with Policy.Decide once per
// device ID (and again only if its IP changes, or after retryAfter when the
// probe failed or the address belonged to no known tailnet node). Devices whose ID is dismissed, already configured, or no
// longer pending produce no prompt, and a prompt is dropped as soon as the
// device leaves the pending list or appears in the configuration. Folder
// offers are reported only when they come from a configured device.
//
// Callbacks run on the goroutine that calls Check or Run, never under the
// Watcher's lock.
type Watcher struct {
	S *Service
	// Dismissed reports whether requests from a device ID were declined for
	// good (prefs.dismissedDevices). Nil means none are dismissed.
	Dismissed func(deviceID string) bool
	// OnPrompt is called once for each newly verified pairing request.
	OnPrompt func(model.PendingDevice)
	// OnIgnore is called once per device left to Syncthing's web UI; Reason
	// says why (for the log).
	OnIgnore func(model.PendingDevice)
	// OnFolder is called once for each new folder offer from a configured device.
	OnFolder func(model.PendingFolder)
	// OnChange is called after a check that changed Pending or PendingFolders.
	OnChange func()
	// Interval is the fallback poll interval; WatchInterval when zero.
	Interval time.Duration

	once    sync.Once
	checkMu sync.Mutex // serialises Check
	kick    chan struct{}
	now     func() time.Time

	mu      sync.Mutex
	seen    map[string]verdict // by device ID
	prompts map[string]model.PendingDevice
	offers  map[folderKey]model.PendingFolder
}

type verdict struct {
	ip        netip.Addr
	action    string
	reason    string
	decidedAt time.Time
}

type folderKey struct{ folder, device string }

// NewWatcher returns a Watcher for s. dismissed may be nil.
func NewWatcher(s *Service, dismissed func(string) bool) *Watcher {
	w := &Watcher{S: s, Dismissed: dismissed}
	w.init()
	return w
}

func (w *Watcher) init() {
	w.once.Do(func() {
		w.kick = make(chan struct{}, 1)
		if w.now == nil {
			w.now = time.Now
		}
		w.seen = map[string]verdict{}
		w.prompts = map[string]model.PendingDevice{}
		w.offers = map[folderKey]model.PendingFolder{}
	})
}

// Kick requests a check soon (for example on a PendingDevicesChanged event).
// It never blocks.
func (w *Watcher) Kick() {
	w.init()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run checks immediately, then on every Kick and every Interval, until ctx
// ends. Check errors (Syncthing or Tailscale briefly unavailable) are
// retried on the next round; Run returns ctx.Err().
func (w *Watcher) Run(ctx context.Context) error {
	w.init()
	iv := w.Interval
	if iv <= 0 {
		iv = WatchInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		_ = w.Check(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-w.kick:
		}
	}
}

// Pending returns the current verified pairing requests, sorted by device ID.
func (w *Watcher) Pending() []model.PendingDevice {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]model.PendingDevice, 0, len(w.prompts))
	for _, p := range w.prompts {
		out = append(out, clonePending(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

// PendingFolders returns the current folder offers from configured devices.
func (w *Watcher) PendingFolders() []model.PendingFolder {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]model.PendingFolder, 0, len(w.offers))
	for _, f := range w.offers {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FolderID != out[j].FolderID {
			return out[i].FolderID < out[j].FolderID
		}
		return out[i].FromDevice < out[j].FromDevice
	})
	return out
}

// Accept pairs with the verified request currently prompted for deviceID and
// drops the prompt. The request is looked up in the Watcher's own prompts,
// which only ever hold devices that passed Policy.Decide; an ID without a
// prompt is refused with ErrNotVerified. UI and API handlers must call this
// with nothing but the device ID, never with a PendingDevice built from
// request data, so a forged "verified" flag cannot bypass the policy. It
// returns the accepted request (for naming the device in notices).
func (w *Watcher) Accept(ctx context.Context, deviceID string) (model.PendingDevice, error) {
	w.init()
	w.mu.Lock()
	pd, ok := w.prompts[deviceID]
	w.mu.Unlock()
	if !ok || !pd.Verified {
		return model.PendingDevice{}, ErrNotVerified
	}
	pd = clonePending(pd)
	if err := w.S.Accept(ctx, pd); err != nil {
		return model.PendingDevice{}, err
	}
	w.Resolve(deviceID)
	return pd, nil
}

// Resolve drops the prompt for deviceID right away (after Accept or
// Decline), without waiting for the next check.
func (w *Watcher) Resolve(deviceID string) {
	w.init()
	w.mu.Lock()
	_, had := w.prompts[deviceID]
	delete(w.prompts, deviceID)
	w.mu.Unlock()
	if had && w.OnChange != nil {
		w.OnChange()
	}
}

type pendingDeviceJSON struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type pendingFolderJSON struct {
	OfferedBy map[string]struct {
		Label string `json:"label"`
	} `json:"offeredBy"`
}

// Check reads the pending lists once and updates prompts and offers. A
// pending device that cannot be evaluated now (tailscale unavailable) is
// retried on the next check; the first such error is returned.
func (w *Watcher) Check(ctx context.Context) error {
	w.init()
	w.checkMu.Lock()
	defer w.checkMu.Unlock()
	s := w.S
	var devs map[string]pendingDeviceJSON
	if err := s.C.Get(ctx, "/rest/cluster/pending/devices", &devs); err != nil {
		return err
	}
	var folders map[string]pendingFolderJSON
	if err := s.C.Get(ctx, "/rest/cluster/pending/folders", &folders); err != nil {
		return err
	}
	configured, err := s.configuredDevices(ctx)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(devs))
	for id := range devs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var (
		firstErr   error
		ts         tailnet.Status
		tsLoaded   bool
		tsErr      error
		newPrompts []model.PendingDevice
		newIgnores []model.PendingDevice
		live       = map[string]bool{}
	)
	for _, id := range ids {
		if !deviceid.Valid(id) || configured[id] || (w.Dismissed != nil && w.Dismissed(id)) {
			continue
		}
		live[id] = true
		pd := model.PendingDevice{DeviceID: id, Name: devs[id].Name, Addr: parseAddrPort(devs[id].Address)}
		ip := pd.Addr.Addr()

		w.mu.Lock()
		v, ok := w.seen[id]
		w.mu.Unlock()
		if ok && v.ip == ip && !(retryable(v.reason) && w.now().Sub(v.decidedAt) >= retryAfter) {
			continue
		}

		var d Decision
		if pre := s.Policy.Decide(pd, tailnet.Status{}, "", nil); pre.Reason == ReasonNotTailnet || pre.Reason == ReasonNoPendingAddr {
			d = pre // no need to ask Tailscale or probe
		} else {
			if !tsLoaded {
				ts, tsErr = s.tailnetStatus(ctx)
				tsLoaded = true
			}
			if tsErr != nil {
				if firstErr == nil {
					firstErr = tsErr
				}
				continue
			}
			var probed string
			var perr error
			if _, known := ts.FindByIP(ip); known {
				pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
				probed, perr = s.probe()(pctx, netip.AddrPortFrom(ip, SyncPort))
				cancel()
				if errors.Is(perr, context.Canceled) && ctx.Err() != nil {
					return ctx.Err()
				}
			}
			d = s.Policy.Decide(pd, ts, probed, perr)
		}

		w.mu.Lock()
		w.seen[id] = verdict{ip: ip, action: d.Action, reason: d.Reason, decidedAt: w.now()}
		w.mu.Unlock()

		if d.Action == ActionPrompt {
			pd.Verified = true
			c := candidateFor(ts, *d.Node)
			c.DeviceID = id
			c.Status = StatusReady
			pd.Node = &c
			pd.Reason = d.Reason
			newPrompts = append(newPrompts, pd)
			continue
		}
		if ok && v.action == ActionIgnore && v.reason == d.Reason {
			continue // same verdict as before: already reported
		}
		pd.Reason = d.Reason
		if d.Node != nil {
			c := candidateFor(ts, *d.Node)
			pd.Node = &c
		}
		newIgnores = append(newIgnores, pd)
	}

	var newOffers []model.PendingFolder
	liveOffers := map[folderKey]model.PendingFolder{}
	for fid, f := range folders {
		for dev, o := range f.OfferedBy {
			if !configured[dev] {
				continue
			}
			liveOffers[folderKey{fid, dev}] = model.PendingFolder{FolderID: fid, Label: o.Label, FromDevice: dev}
		}
	}

	changed := false
	w.mu.Lock()
	for id := range w.seen {
		if !live[id] {
			delete(w.seen, id)
		}
	}
	for id := range w.prompts {
		if !live[id] {
			delete(w.prompts, id)
			changed = true
		}
	}
	for _, pd := range newPrompts {
		w.prompts[pd.DeviceID] = pd
		changed = true
	}
	for _, pd := range newIgnores {
		if _, had := w.prompts[pd.DeviceID]; had {
			// A re-evaluation (new IP) that no longer passes removes the prompt.
			delete(w.prompts, pd.DeviceID)
			changed = true
		}
	}
	for k := range w.offers {
		if _, ok := liveOffers[k]; !ok {
			delete(w.offers, k)
			changed = true
		}
	}
	for k, f := range liveOffers {
		if _, ok := w.offers[k]; !ok {
			w.offers[k] = f
			newOffers = append(newOffers, f)
			changed = true
		}
	}
	w.mu.Unlock()

	sort.Slice(newOffers, func(i, j int) bool {
		if newOffers[i].FolderID != newOffers[j].FolderID {
			return newOffers[i].FolderID < newOffers[j].FolderID
		}
		return newOffers[i].FromDevice < newOffers[j].FromDevice
	})
	for _, pd := range newIgnores {
		if w.OnIgnore != nil {
			w.OnIgnore(clonePending(pd))
		}
	}
	for _, pd := range newPrompts {
		if w.OnPrompt != nil {
			w.OnPrompt(clonePending(pd))
		}
	}
	for _, f := range newOffers {
		if w.OnFolder != nil {
			w.OnFolder(f)
		}
	}
	if changed && w.OnChange != nil {
		w.OnChange()
	}
	return firstErr
}

func clonePending(p model.PendingDevice) model.PendingDevice {
	if p.Node != nil {
		c := *p.Node
		p.Node = &c
	}
	return p
}

// parseAddrPort parses Syncthing's pending "address" ("ip:port", optionally
// with a scheme such as "tcp://").
func parseAddrPort(s string) netip.AddrPort {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}
