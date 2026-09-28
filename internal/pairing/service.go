package pairing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

// Candidate statuses (model.Candidate.Status).
const (
	StatusReady       = "ready"
	StatusPaired      = "paired"
	StatusSelf        = "self"
	StatusBlocked     = "blocked"
	StatusNoSyncthing = "no-syncthing"
)

// MaxParallelProbes bounds concurrent probes during discovery.
const MaxParallelProbes = 4

// DynamicAddress is Syncthing's "use discovery" device address.
const DynamicAddress = "dynamic"

// Errors returned by Service.
var (
	// ErrTailscaleNotRunning: Tailscale is installed but not connected and logged in.
	ErrTailscaleNotRunning = errors.New("Tailscale is not connected")
	// ErrNotVerified: Accept was called for a request that did not pass Policy.Decide.
	ErrNotVerified = errors.New("this pairing request was not verified over Tailscale")
	// ErrNotTailnetAddress: an address outside the tailnet prefixes was given.
	ErrNotTailnetAddress = errors.New("address is not a tailnet address")
	// ErrNotConfigured: the device is not configured in Syncthing.
	ErrNotConfigured = errors.New("device is not configured in Syncthing")
	// ErrSelf: the device ID is this Syncthing's own ID.
	ErrSelf = errors.New("this is the local device")
)

// Service performs pairing actions against the local Syncthing.
//
// Probe defaults to the package-level Probe when nil. Prefs is optional:
// when set, Decline records the device in dismissedDevices and Widen records
// widened[id] so it runs only once.
type Service struct {
	C      *stclient.Client
	TS     tailnet.Source
	Policy Policy
	Probe  func(context.Context, netip.AddrPort) (string, error)
	Prefs  *prefs.Store
}

// NewService returns a Service with the production policy (tailnet.Prefixes)
// and prober.
func NewService(c *stclient.Client, ts tailnet.Source, p *prefs.Store) *Service {
	return &Service{C: c, TS: ts, Policy: Policy{Prefixes: tailnet.Prefixes}, Probe: Probe, Prefs: p}
}

func (s *Service) probe() func(context.Context, netip.AddrPort) (string, error) {
	if s.Probe != nil {
		return s.Probe
	}
	return Probe
}

// tailnetStatus returns the tailnet status, or ErrTailscaleNotRunning (wrapped, with
// the backend state) when Tailscale is not connected.
func (s *Service) tailnetStatus(ctx context.Context) (tailnet.Status, error) {
	if s.TS == nil {
		return tailnet.Status{}, tailnet.ErrNotInstalled
	}
	st, err := s.TS.Status(ctx)
	if err != nil {
		return tailnet.Status{}, err
	}
	if !st.Running() {
		return st, fmt.Errorf("%w (state %q)", ErrTailscaleNotRunning, st.BackendState)
	}
	return st, nil
}

// Discover lists the tailnet peers that could be paired and probes each for
// its Syncthing device ID (at most MaxParallelProbes at once, ProbeTimeout
// each). It is meant to run only at startup and on explicit user action.
func (s *Service) Discover(ctx context.Context) ([]model.Candidate, error) {
	st, err := s.tailnetStatus(ctx)
	if err != nil {
		return nil, err
	}
	myID, err := s.myID(ctx)
	if err != nil {
		return nil, err
	}
	configured, err := s.configuredDevices(ctx)
	if err != nil {
		return nil, err
	}

	nodes := st.Candidates()
	out := make([]model.Candidate, len(nodes))
	probe := s.probe()
	sem := make(chan struct{}, MaxParallelProbes)
	var wg sync.WaitGroup
	for i, n := range nodes {
		out[i] = candidateFor(st, n)
		ip, ok := n.IPv4()
		if !ok {
			ip, ok = n.PreferredIP()
		}
		if !ok {
			out[i].Status = StatusNoSyncthing
			continue
		}
		wg.Add(1)
		go func(i int, ap netip.AddrPort) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i].Status = StatusNoSyncthing
				return
			}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			defer cancel()
			id, perr := probe(pctx, ap)
			out[i].DeviceID = id
			out[i].Status = candidateStatus(id, perr, myID, configured)
		}(i, netip.AddrPortFrom(ip, SyncPort))
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func candidateStatus(id string, err error, myID string, configured map[string]bool) string {
	switch {
	case err == nil && id != "" && id == myID:
		return StatusSelf
	case err == nil && id != "" && configured[id]:
		return StatusPaired
	case err == nil && id != "":
		return StatusReady
	case errors.Is(err, ErrRefused):
		return StatusBlocked
	default:
		return StatusNoSyncthing
	}
}

// Pair adds candidate c to the local Syncthing with its tailnet address, so
// that Syncthing dials it. The remote side still has to accept.
func (s *Service) Pair(ctx context.Context, c model.Candidate) error {
	id, err := deviceid.Parse(c.DeviceID)
	if err != nil {
		return err
	}
	if c.Status == StatusSelf {
		return ErrSelf
	}
	return s.upsertDevice(ctx, id, c.NodeName, c.IP)
}

// Accept adds a verified pending device (see Watcher and Policy.Decide) with
// its tailnet address. Unverified requests are refused.
//
// Accept trusts pd, including its Verified flag and tailnet address, so pd
// must come from the Watcher, never from request data. UI and API handlers
// use Watcher.Accept, which takes only a device ID.
func (s *Service) Accept(ctx context.Context, pd model.PendingDevice) error {
	if !pd.Verified {
		return ErrNotVerified
	}
	id, err := deviceid.Parse(pd.DeviceID)
	if err != nil {
		return err
	}
	ip := pd.Addr.Addr()
	name := pd.Name
	if pd.Node != nil {
		if pd.Node.IP.IsValid() {
			ip = pd.Node.IP
		}
		if pd.Node.NodeName != "" {
			name = pd.Node.NodeName
		}
	}
	return s.upsertDevice(ctx, id, name, ip)
}

// Decline removes a pending device request and remembers the ID so it is not
// offered again.
func (s *Service) Decline(ctx context.Context, deviceID string) error {
	id, err := deviceid.Parse(deviceID)
	if err != nil {
		return err
	}
	if err := s.C.Send(ctx, http.MethodDelete, "/rest/cluster/pending/devices?"+url.Values{"device": {id}}.Encode(), nil); err != nil {
		return err
	}
	if s.Prefs != nil {
		return s.Prefs.DismissDevice(id)
	}
	return nil
}

// Widen appends "dynamic" to the device's addresses (once), so LAN, local
// discovery and, in the hybrid profile, global discovery also work after the
// first tailnet connection.
func (s *Service) Widen(ctx context.Context, deviceID string) error {
	id, err := deviceid.Parse(deviceID)
	if err != nil {
		return err
	}
	if s.Prefs != nil && s.Prefs.Get().IsWidened(id) {
		return nil
	}
	dev, found, err := s.getDevice(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotConfigured
	}
	addrs := stringList(dev["addresses"])
	if !slices.Contains(addrs, DynamicAddress) {
		addrs = append(addrs, DynamicAddress)
		if err := s.C.Send(ctx, http.MethodPatch, devicePath(id), map[string]any{"addresses": addrs}); err != nil {
			return err
		}
	}
	if s.Prefs != nil {
		return s.Prefs.MarkWidened(id)
	}
	return nil
}

// tailnetAddresses returns the tcp:// and quic:// addresses for ip:22000.
func tailnetAddresses(ip netip.Addr) []string {
	ap := netip.AddrPortFrom(ip, SyncPort).String()
	return []string{"tcp://" + ap, "quic://" + ap}
}

// upsertDevice configures id with the tailnet address of ip through
// PUT /rest/config/devices/{id}. An existing device keeps all its settings;
// only missing tailnet addresses are added.
func (s *Service) upsertDevice(ctx context.Context, id, name string, ip netip.Addr) error {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() || !s.Policy.contains(ip) {
		return fmt.Errorf("%w: %v", ErrNotTailnetAddress, ip)
	}
	myID, err := s.myID(ctx)
	if err != nil {
		return err
	}
	if id == myID {
		return ErrSelf
	}
	want := tailnetAddresses(ip)
	dev, found, err := s.getDevice(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		if name == "" {
			name = ip.String()
		}
		return s.C.Send(ctx, http.MethodPut, devicePath(id), map[string]any{
			"deviceID":          id,
			"name":              name,
			"addresses":         want,
			"autoAcceptFolders": false,
			"introducer":        false,
			"compression":       "metadata",
		})
	}
	addrs := stringList(dev["addresses"])
	changed := false
	for _, a := range want {
		if !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
			changed = true
		}
	}
	if n, _ := dev["name"].(string); n == "" && name != "" {
		dev["name"] = name
		changed = true
	}
	if !changed {
		return nil
	}
	dev["addresses"] = addrs
	dev["deviceID"] = id
	return s.C.Send(ctx, http.MethodPut, devicePath(id), dev)
}

func devicePath(id string) string { return "/rest/config/devices/" + url.PathEscape(id) }

func folderPath(id string) string { return "/rest/config/folders/" + url.PathEscape(id) }

// getDevice fetches one configured device as a generic object, so a PUT back
// keeps every field, including ones this code does not know about.
func (s *Service) getDevice(ctx context.Context, id string) (map[string]any, bool, error) {
	var dev map[string]any
	err := s.C.Get(ctx, devicePath(id), &dev)
	if isNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return dev, dev != nil, nil
}

func isNotFound(err error) bool {
	return err != nil && stclient.StatusOf(err) == http.StatusNotFound
}

func (s *Service) myID(ctx context.Context) (string, error) {
	var st struct {
		MyID string `json:"myID"`
	}
	if err := s.C.Get(ctx, "/rest/system/status", &st); err != nil {
		return "", err
	}
	id, err := deviceid.Parse(st.MyID)
	if err != nil {
		return "", fmt.Errorf("local Syncthing reported an invalid device ID: %w", err)
	}
	return id, nil
}

// configuredDevices returns the IDs of all devices in the Syncthing config
// (including this device).
func (s *Service) configuredDevices(ctx context.Context) (map[string]bool, error) {
	var devs []struct {
		DeviceID string `json:"deviceID"`
	}
	if err := s.C.Get(ctx, "/rest/config/devices", &devs); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(devs))
	for _, d := range devs {
		out[d.DeviceID] = true
	}
	return out, nil
}

// stringList converts a decoded JSON array of strings; other values are dropped.
func stringList(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr)+3)
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
