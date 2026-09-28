package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/notify"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
	"github.com/Neutx/syncthing-v2/internal/update"
)

// API is the part of the Syncthing REST client the checks use.
type API interface {
	Get(ctx context.Context, path string, out any) error
}

// ErrUpdatesOff is returned by Env.Update when the user turned update checks
// off; UPD001 is then skipped.
var ErrUpdatesOff = errors.New("update checks are turned off in Settings")

// ErrNoPrefs is returned by the default Env.Update before the first launch:
// without prefs.json there is nowhere to record the 24 h throttle, so UPD001
// is skipped instead of querying GitHub on every doctor run.
var ErrNoPrefs = errors.New("no preferences yet; run " + brand.DisplayName + " once")

// Env is everything the checks read. A nil function skips the checks that
// need it (with a reason), so a partial Env is valid.
type Env struct {
	// GOOS selects the per-OS checks (FW001 and UI001 on Windows, UI002 on
	// Linux).
	GOOS string
	// Syncthing finds the Syncthing binary (stinstall.Detect).
	Syncthing func(ctx context.Context) (stinstall.Install, error)
	// Discover finds and reads the config Syncthing uses (stclient.Discover).
	Discover func(ctx context.Context, bin string) (stclient.Endpoint, string, error)
	// ReadConfig reads the GUI section for the SEC001 audit (stclient.ReadConfig).
	ReadConfig func(path string) (stclient.Endpoint, stclient.GUIConfig, error)
	// API opens a REST client for an endpoint (stclient.New).
	API func(ep stclient.Endpoint) API
	// Tailscale returns the tailnet status; an error wrapping
	// tailnet.ErrNotInstalled means the CLI is missing.
	Tailscale func(ctx context.Context) (tailnet.Status, error)
	// Probe reads the device ID of a Syncthing sync listener (pairing.Probe).
	Probe func(ctx context.Context, ap netip.AddrPort) (string, error)
	// FirewallRule reports whether our Windows Firewall rule exists.
	FirewallRule func(ctx context.Context) (bool, error)
	// WebView2 reports whether the WebView2 runtime is installed.
	WebView2 func() bool
	// StatusNotifier reports whether a StatusNotifierWatcher owns its bus name.
	StatusNotifier func() bool
	// Update returns the release check result, or ErrUpdatesOff.
	Update func(ctx context.Context) (update.Result, error)
}

func (e Env) withDefaults() Env {
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	return e
}

// DefaultEnv wires the real system: stinstall, stclient, the tailscale CLI,
// pairing.Probe, netsh, the WebView2 loader, the session bus and the
// throttled GitHub release check.
func DefaultEnv() Env {
	return Env{
		GOOS:       runtime.GOOS,
		Syncthing:  stinstall.Detect,
		Discover:   stclient.Discover,
		ReadConfig: stclient.ReadConfig,
		API:        func(ep stclient.Endpoint) API { return stclient.New(ep) },
		Tailscale: func(ctx context.Context) (tailnet.Status, error) {
			p, err := tailnet.Locate()
			if err != nil {
				return tailnet.Status{}, err
			}
			return tailnet.CLI{Path: p}.Status(ctx)
		},
		Probe:          pairing.Probe,
		FirewallRule:   install.HasFirewallRule,
		WebView2:       install.WebView2Available,
		StatusNotifier: notify.HasStatusNotifierWatcher,
		Update:         defaultUpdate,
	}
}

// defaultUpdate runs the same throttled check as the tray (at most one
// GitHub query per 24 h), honouring the Settings switch.
func defaultUpdate(ctx context.Context) (update.Result, error) {
	dir, err := osutil.AppDataDir()
	if err != nil {
		return update.Result{}, err
	}
	return updateFrom(ctx, dir, update.New)
}

// updateFrom runs the release check with the prefs in dataDir. Before the
// first launch there are no prefs, and creating them here would skip the
// first-run setup, so it returns ErrNoPrefs without any network access.
func updateFrom(ctx context.Context, dataDir string, newChecker func(*prefs.Store) *update.Checker) (update.Result, error) {
	if _, err := os.Stat(filepath.Join(dataDir, prefs.FileName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return update.Result{}, ErrNoPrefs
		}
		return update.Result{}, err
	}
	st, err := prefs.Open(dataDir)
	if st == nil {
		return update.Result{}, err
	}
	if !st.Get().CheckForUpdates {
		return update.Result{}, ErrUpdatesOff
	}
	return newChecker(st).Check(ctx)
}

// results collects findings (by code) and skip reasons.
type results struct {
	mu      sync.Mutex
	found   map[string][]string
	skipped map[string]string
}

func (r *results) add(code, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.found[code] = append(r.found[code], msg)
}

func (r *results) skip(reason string, codes ...string) {
	for _, c := range codes {
		if _, ok := r.skipped[c]; !ok {
			r.skipped[c] = reason
		}
	}
}

func runChecks(ctx context.Context, env Env) *results {
	r := &results{found: map[string][]string{}, skipped: map[string]string{}}
	ts, tsOK := checkTailscale(ctx, env, r)
	checkSyncthing(ctx, env, r)
	checkNetwork(ctx, env, r, ts, tsOK)
	checkPlatform(ctx, env, r)
	checkUpdate(ctx, env, r)
	return r
}

// TS001, TS002.
func checkTailscale(ctx context.Context, env Env, r *results) (tailnet.Status, bool) {
	if env.Tailscale == nil {
		r.skip("no Tailscale source", "TS001", "TS002")
		return tailnet.Status{}, false
	}
	st, err := env.Tailscale(ctx)
	switch {
	case errors.Is(err, tailnet.ErrNotInstalled):
		r.add("TS001", "The tailscale command-line tool was not found. Install Tailscale from https://tailscale.com/download and sign in.")
		r.skip("Tailscale is not installed", "TS002")
		return st, false
	case err != nil:
		r.add("TS002", "Tailscale did not report its status ("+err.Error()+"). Make sure the Tailscale service is running.")
		return st, false
	case !st.Running():
		state := st.BackendState
		if state == "" {
			state = "not running"
		}
		r.add("TS002", "Tailscale is "+state+", not connected. Open Tailscale and sign in.")
		return st, false
	}
	return st, true
}

// ST001–ST005, SEC001.
func checkSyncthing(ctx context.Context, env Env, r *results) {
	all := []string{"ST001", "ST002", "ST003", "ST004", "ST005", "SEC001"}
	if env.Syncthing == nil {
		r.skip("no Syncthing detector", all...)
		return
	}
	inst, err := env.Syncthing(ctx)
	if err != nil {
		msg := "Syncthing was not found. Open " + brand.DisplayName + " to install it, or install Syncthing yourself."
		if !errors.Is(err, stinstall.ErrNotFound) {
			msg = "Syncthing could not be located (" + err.Error() + ")."
		}
		r.add("ST001", msg)
		r.skip("Syncthing is not installed", all[1:]...)
		return
	}
	// ST004 is decided on return: when `syncthing --version` gave nothing, a
	// running Syncthing can still report its version over the REST API. When
	// it is not answering (ST002/ST003 reported), an unknown version has the
	// same cause, so it is skipped rather than reported twice.
	version := inst.Version
	defer func() {
		if strings.TrimSpace(version) == "" && (len(r.found["ST002"]) > 0 || len(r.found["ST003"]) > 0) {
			r.skip("the version is unknown while Syncthing is not answering", "ST004")
			return
		}
		if ok, msg := stinstall.CheckVersion(version); !ok {
			r.add("ST004", msg)
		}
	}()

	if env.Discover == nil {
		r.skip("no config discovery", "ST002", "ST003", "ST005", "SEC001")
		return
	}
	ep, cfgPath, err := env.Discover(ctx, inst.Bin)
	if err != nil {
		if errors.Is(err, stclient.ErrConfigNotFound) {
			r.add("ST002", "Syncthing has no configuration yet, so it is not running. Open "+brand.DisplayName+" to set it up.")
		} else {
			r.add("ST002", "Syncthing's configuration could not be read ("+err.Error()+").")
		}
		r.skip("Syncthing's configuration is unavailable", "ST003", "ST005", "SEC001")
		return
	}

	if env.ReadConfig == nil {
		r.skip("no config reader", "SEC001")
	} else if _, gui, err := env.ReadConfig(cfgPath); err != nil {
		r.skip("the GUI settings could not be read", "SEC001")
	} else {
		for _, f := range stclient.Audit(gui) {
			if f.Code == "SEC001" {
				r.add("SEC001", f.Text+" Restrict it to this computer from the "+brand.DisplayName+" dashboard, or set a GUI password in Syncthing.")
			}
		}
	}

	if env.API == nil {
		r.skip("no REST client", "ST003", "ST005")
		return
	}
	api := env.API(ep)
	var status struct {
		MyID string `json:"myID"`
	}
	if err := api.Get(ctx, "/rest/system/status", &status); err != nil {
		kind, typed := stclient.KindOf(err)
		switch {
		case typed && kind == stclient.ErrUnauthorized:
			r.add("ST003", "Syncthing rejected the API key from "+cfgPath+". Restart Syncthing so it reloads its configuration.")
		case typed && kind == stclient.ErrBadResponse:
			r.add("ST002", "Syncthing at "+hostOf(ep)+" gave an unexpected response ("+err.Error()+").")
			r.skip("Syncthing is not answering", "ST003")
		default:
			r.add("ST002", "Syncthing is not responding on "+hostOf(ep)+". Start it from the "+brand.DisplayName+" menu.")
			r.skip("Syncthing is not answering", "ST003")
		}
		r.skip("Syncthing is not answering", "ST005")
		return
	}
	if strings.TrimSpace(version) == "" {
		var v struct {
			Version string `json:"version"`
		}
		if err := api.Get(ctx, "/rest/system/version", &v); err == nil {
			version = v.Version
		}
	}

	var opts struct {
		ListenAddresses []string `json:"listenAddresses"`
	}
	if err := api.Get(ctx, "/rest/config/options", &opts); err != nil {
		r.skip("the listen addresses could not be read", "ST005")
		return
	}
	if !listensOnDefault(opts.ListenAddresses) {
		r.add("ST005", "Syncthing listens for sync connections on "+strings.Join(opts.ListenAddresses, ", ")+
			", not on port 22000. Pairing probes other devices on 22000, so they cannot find this one; pair it manually or use the default port.")
	}
}

func hostOf(ep stclient.Endpoint) string {
	if ep.BaseURL != nil {
		return ep.BaseURL.Host
	}
	return "its configured address"
}

// listensOnDefault reports whether a Syncthing listenAddresses list accepts
// sync connections on port 22000 ("default" does, over TCP and QUIC).
func listensOnDefault(addrs []string) bool {
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "default" {
			return true
		}
		u, err := url.Parse(a)
		if err != nil {
			continue
		}
		switch strings.ToLower(u.Scheme) {
		case "tcp", "tcp4", "tcp6", "quic", "quic4", "quic6":
		default:
			continue
		}
		if _, port, err := net.SplitHostPort(u.Host); err == nil && port == "22000" {
			return true
		}
	}
	return false
}

// probeParallel is the number of concurrent NET001 probes (§4.2).
const probeParallel = 4

// NET001.
func checkNetwork(ctx context.Context, env Env, r *results, ts tailnet.Status, tsOK bool) {
	switch {
	case !tsOK:
		r.skip("Tailscale is not connected", "NET001")
		return
	case env.Probe == nil:
		r.skip("no probe", "NET001")
		return
	}
	type target struct {
		name string
		ap   netip.AddrPort
	}
	var targets []target
	for _, n := range ts.Candidates() {
		ip, ok := n.IPv4()
		if !ts.SameOwner(n) || !ok {
			continue
		}
		targets = append(targets, target{name: n.HostName, ap: netip.AddrPortFrom(ip, pairing.SyncPort)})
	}
	msgs := make([]string, len(targets))
	sem := make(chan struct{}, probeParallel)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, pairing.ProbeTimeout)
			defer cancel()
			if _, err := env.Probe(pctx, t.ap); err != nil {
				msgs[i] = fmt.Sprintf("Syncthing on %s (%s) is not reachable: %s. Check your Tailscale ACLs and the firewall there, or install %s on it.",
					t.name, t.ap, probeReason(err), brand.DisplayName)
			}
		}()
	}
	wg.Wait()
	for _, m := range msgs {
		if m != "" {
			r.add("NET001", m)
		}
	}
}

func probeReason(err error) string {
	switch {
	case errors.Is(err, pairing.ErrRefused):
		return "the connection was refused"
	case errors.Is(err, pairing.ErrTimeout):
		return "the connection timed out"
	case errors.Is(err, pairing.ErrNoSyncthing):
		return "no Syncthing answered"
	}
	return err.Error()
}

// FW001, UI001, UI002.
func checkPlatform(ctx context.Context, env Env, r *results) {
	if env.GOOS != "windows" {
		r.skip("Windows only", "FW001", "UI001")
	} else {
		if env.FirewallRule == nil {
			r.skip("no firewall reader", "FW001")
		} else if has, err := env.FirewallRule(ctx); err != nil {
			r.skip("the firewall rules could not be read ("+err.Error()+")", "FW001")
		} else if !has {
			r.add("FW001", `The Windows Firewall rule "`+install.FirewallRuleName+`" is not present, so Windows may block other devices from connecting to Syncthing. Run "`+brand.BinaryName+` firewall allow" (it asks for administrator rights). Pairing needs it: other devices check a pairing request from this computer by connecting back to it on port 22000.`)
		}
		if env.WebView2 == nil {
			r.skip("no WebView2 detector", "UI001")
		} else if !env.WebView2() {
			r.add("UI001", "The Microsoft Edge WebView2 runtime is not installed, so the dashboard opens in your web browser instead. Install it from https://developer.microsoft.com/microsoft-edge/webview2/.")
		}
	}
	if env.GOOS != "linux" {
		r.skip("Linux only", "UI002")
	} else if env.StatusNotifier == nil {
		r.skip("no session bus reader", "UI002")
	} else if !env.StatusNotifier() {
		r.add("UI002", "No StatusNotifierWatcher is running on the session bus, so the tray icon cannot be shown. On GNOME, install the \"AppIndicator and KStatusNotifierItem Support\" extension; the dashboard opens in the browser meanwhile.")
	}
}

// UPD001.
func checkUpdate(ctx context.Context, env Env, r *results) {
	if env.Update == nil {
		r.skip("no release checker", "UPD001")
		return
	}
	res, err := env.Update(ctx)
	switch {
	case errors.Is(err, ErrUpdatesOff):
		r.skip(ErrUpdatesOff.Error(), "UPD001")
	case errors.Is(err, ErrNoPrefs):
		r.skip(ErrNoPrefs.Error(), "UPD001")
	case err != nil:
		r.skip("the release check failed ("+err.Error()+")", "UPD001")
	case res.Available:
		r.add("UPD001", brand.DisplayName+" "+res.Latest.Version+" is available (this is "+displayVersion(brand.Version)+"). Download it from "+res.Latest.URL+".")
	}
}

func displayVersion(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	if _, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); err == nil {
		return "v" + v
	}
	return v
}
