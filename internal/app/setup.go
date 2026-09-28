package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/status"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

// apiWait bounds how long a freshly started Syncthing may take to answer.
const apiWait = 60 * time.Second

// startDebounce keeps a second "Start Syncthing" from launching another
// process while the first one is still coming up (F25).
const startDebounce = 20 * time.Second

// autostartOffStopsSyncthing is true where turning SyncThing V2's own Syncthing
// login entry off also stops a Syncthing the service manager runs: on macOS
// the KeepAlive launchd job is booted out (package autostart).
var autostartOffStopsSyncthing = runtime.GOOS == "darwin"

// ---------- Tailscale ----------

// lazyTailnet locates the tailscale CLI on every call, so installing
// Tailscale while the tray runs needs no restart.
type lazyTailnet struct{}

func (lazyTailnet) Status(ctx context.Context) (tailnet.Status, error) {
	p, err := tailnet.Locate()
	if err != nil {
		return tailnet.Status{}, err
	}
	return tailnet.CLI{Path: p}.Status(ctx)
}

// Tailnet returns the production tailnet source: the tailscale CLI, looked
// up on each call.
func Tailnet() tailnet.Source { return lazyTailnet{} }

// checkTailscale sets the "tailscale-missing" notice when the CLI is absent.
func (a *App) checkTailscale() {
	_, err := a.locateTS()
	missing := errors.Is(err, tailnet.ErrNotInstalled)
	a.mu.Lock()
	changed := missing != a.tsMissing
	a.tsMissing = missing
	a.mu.Unlock()
	if changed {
		a.publish()
	}
}

// ---------- legacy prototype (§6.5) ----------

// checkLegacy raises the one-time "legacy-tray" notice when the prototype
// tray is installed or running and the question was never answered.
func (a *App) checkLegacy() {
	if a.prefs.Get().LegacyAsked {
		return
	}
	r, err := a.roots()
	if err != nil {
		return
	}
	found := install.DetectLegacy(r).Found()
	a.mu.Lock()
	a.legacy = found
	a.mu.Unlock()
	if found {
		applog.Printf("app: the legacy Syncthing tray was found")
		a.publish()
	}
}

// ---------- Syncthing discovery and sessions ----------

// Connect finds Syncthing and opens a REST client for the config it uses,
// for the one-shot CLI commands. A missing binary is not an error as long
// as a config is found (STHOMEDIR, or the default location).
func Connect(ctx context.Context) (*stclient.Client, stinstall.Install, error) {
	inst, err := stinstall.Detect(ctx)
	if err != nil && !errors.Is(err, stinstall.ErrNotFound) {
		return nil, inst, err
	}
	ep, _, err := stclient.Discover(ctx, inst.Bin)
	if err != nil {
		return nil, inst, err
	}
	return stclient.New(ep), inst, nil
}

// connectLoop connects at once and then retries while there is no working
// connection.
func (a *App) connectLoop() {
	a.connect(a.ctx)
	t := time.NewTicker(a.reconnectIn)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		need := a.sess == nil || !a.haveBase ||
			a.base.State == model.StateDown || a.base.State == model.StateUnauthorized
		busy := a.booting
		a.mu.Unlock()
		if need && !busy {
			a.connect(a.ctx)
		}
	}
}

// connect detects Syncthing (once found, the binary is kept), discovers the
// config it uses, and attaches a new session when the endpoint changed.
// Calls are serialised, so a second caller sees the session the first one
// attached instead of replacing it.
func (a *App) connect(ctx context.Context) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.mu.Lock()
	inst, found := a.inst, a.instFound
	a.mu.Unlock()
	if !found {
		i, err := a.detect(ctx)
		switch {
		case err == nil:
			inst, found = i, true
			a.mu.Lock()
			a.setInstLocked(i)
			a.mu.Unlock()
			applog.Printf("app: Syncthing %s at %s (managed %v)", i.Version, i.Bin, i.Managed)
			if ok, msg := stinstall.CheckVersion(i.Version); !ok && i.Version != "" {
				applog.Printf("app: %s", msg)
			}
		case errors.Is(err, stinstall.ErrNotFound):
		default:
			applog.Printf("app: looking for Syncthing: %v", err)
		}
	}
	ep, cfg, err := a.discover(ctx, inst.Bin)
	if err != nil {
		reason := "Syncthing is not set up yet. Click Start Syncthing to set it up."
		if !found {
			reason = "Syncthing was not found on this computer. Click Start Syncthing to download and set it up."
		}
		if !errors.Is(err, stclient.ErrConfigNotFound) {
			reason = "Syncthing's configuration could not be read."
			applog.Printf("app: reading the Syncthing config: %v", err)
		}
		a.mu.Lock()
		changed := a.downReason != reason
		a.downReason = reason
		a.mu.Unlock()
		if changed {
			a.publish()
		}
		return
	}
	a.mu.Lock()
	same := a.sess != nil && a.sess.ep.BaseURL.String() == ep.BaseURL.String() && a.sess.ep.APIKey == ep.APIKey
	a.mu.Unlock()
	if !same {
		a.attach(ep, cfg)
	}
}

// attach starts a session on ep, replacing the current one.
func (a *App) attach(ep stclient.Endpoint, cfgPath string) {
	applog.AddSecret(ep.APIKey)
	c := stclient.New(ep)
	ctx, cancel := context.WithCancel(a.ctx)
	s := &session{ep: ep, configPath: cfgPath, client: c, cancel: cancel}
	s.engine = a.newEngine(c, a.startupCached)
	s.svc = pairing.NewService(c, a.ts, a.prefs)
	if a.probe != nil {
		s.svc.Probe = a.probe
	}
	s.watcher = pairing.NewWatcher(s.svc, func(id string) bool { return a.prefs.Get().DeviceDismissed(id) })
	s.watcher.OnPrompt = a.onPairingPrompt
	s.watcher.OnFolder = a.onFolderOffer
	s.watcher.OnIgnore = func(pd model.PendingDevice) {
		applog.Printf("app: pairing request from %s left to Syncthing's web UI: %s", pdShort(pd), pd.Reason)
	}
	s.watcher.OnChange = a.publish

	var gui stclient.GUIConfig
	if cfgPath != "" {
		if _, g, err := a.readConfig(cfgPath); err == nil {
			gui = g
		}
	}
	s.gui = gui
	exposed := false
	for _, f := range stclient.Audit(gui) {
		if f.Code == "SEC001" {
			exposed = true
		}
	}

	a.mu.Lock()
	old := a.sess
	a.sess = s
	a.base, a.haveBase = model.Snapshot{}, false
	a.havePrev = false
	a.pendingSig = ""
	a.connected = map[string]bool{}
	a.guiExposed, a.guiPort = exposed, gui.Port
	a.downReason = ""
	notes := a.notes
	a.notes = nil
	notifySec := exposed && !a.secNotified && !a.prefs.Get().NoticeDismissed(NoticeGUIExposed)
	if notifySec {
		a.secNotified = true
	}
	a.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	applog.Printf("app: connected to Syncthing at %s", c.Host())

	// Notes written while there was no engine move into its activity list,
	// oldest first so the order is kept.
	for i := len(notes) - 1; i >= 0; i-- {
		s.engine.Note(notes[i].Text, notes[i].Tint)
	}
	snaps := s.engine.Run(ctx)
	a.goRun(func() {
		for snap := range snaps {
			a.onEngineSnapshot(s, snap)
		}
	})
	a.goRun(func() { _ = s.watcher.Run(ctx) })
	if notifySec {
		a.notify("Syncthing control panel exposed",
			"Your Syncthing control panel is reachable from other devices without a password. Open "+brand.DisplayName+" to restrict it.")
	}
	a.mu.Lock()
	first := !a.discovered
	a.discovered = true
	a.mu.Unlock()
	if first {
		// Discovery runs once at startup (§4.2), so the Pair view opens with
		// results; later runs happen only on user action.
		a.goRun(func() { a.runDiscovery(ctx, s) })
	}
	a.publish()
}

// session returns the current session, or nil.
func (a *App) session() *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

// ---------- pairing prompts ----------

func pdShort(pd model.PendingDevice) string {
	id := pd.DeviceID
	if len(id) > 7 {
		id = id[:7]
	}
	return id
}

// PromptText is the pairing prompt of §4.3 step 3d.
func PromptText(pd model.PendingDevice) (title, body string) {
	name, osName := pd.Name, ""
	if pd.Node != nil {
		if pd.Node.NodeName != "" {
			name = pd.Node.NodeName
		}
		osName = pd.Node.OS
	}
	if name == "" {
		name = pd.Addr.Addr().String()
	}
	body = name + " wants to sync with this computer"
	if osName != "" {
		body += " · " + osName
	}
	body += " · ID " + pdShort(pd)
	if pd.Node != nil && !pd.Node.SameOwner {
		login := pd.Node.LoginName
		if login == "" {
			login = "another Tailscale user"
		}
		body += ". Owned by " + login + " — only accept if you know this person"
	}
	return "Pairing request", body
}

func (a *App) onPairingPrompt(pd model.PendingDevice) {
	title, body := PromptText(pd)
	applog.Printf("app: verified pairing request from %s", pdShort(pd))
	a.notifyView(title, body, ViewPair)
}

func (a *App) onFolderOffer(pf model.PendingFolder) {
	a.notifyView("Folder shared with you", a.peerName(pf.FromDevice)+" shares \""+folderLabel(pf)+"\"", ViewPair)
}

func folderLabel(pf model.PendingFolder) string {
	if pf.Label != "" {
		return pf.Label
	}
	return pf.FolderID
}

// peerName is the configured name of a device, or its short ID.
func (a *App) peerName(id string) string {
	for _, p := range a.lastSnapshot().Peers {
		if p.ID == id && p.Name != "" {
			return p.Name
		}
	}
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// runDiscovery lists and probes pairing candidates and caches them.
func (a *App) runDiscovery(ctx context.Context, s *session) ([]model.Candidate, error) {
	cands, err := s.svc.Discover(ctx)
	if err != nil {
		if errors.Is(err, tailnet.ErrNotInstalled) {
			a.checkTailscale()
		}
		return nil, err
	}
	a.mu.Lock()
	a.cands = map[string]model.Candidate{}
	for _, c := range cands {
		if c.DeviceID != "" {
			a.cands[c.DeviceID] = c
		}
	}
	a.mu.Unlock()
	a.checkTailscale()
	return cands, nil
}

// ---------- starting and bootstrapping Syncthing (§5.2) ----------

// ensureSyncthing is "Start Syncthing": it starts the detected Syncthing,
// generating a config first when there is none, or downloads, sets up and
// starts one when Syncthing is missing. The download runs in the
// background; progress shows in the dashboard.
func (a *App) ensureSyncthing(ctx context.Context) error {
	a.mu.Lock()
	if a.booting {
		a.mu.Unlock()
		return nil
	}
	found, inst := a.instFound, a.inst
	a.mu.Unlock()
	if !found {
		if i, err := a.detect(ctx); err == nil {
			found, inst = true, i
			a.mu.Lock()
			a.setInstLocked(i)
			a.mu.Unlock()
		}
	}
	if !found {
		a.goRun(func() {
			if err := a.bootstrap(a.ctx); err != nil {
				applog.Printf("app: setting up Syncthing: %v", err)
			}
		})
		return nil
	}
	return a.startSyncthing(ctx, inst)
}

// startSyncthing starts the detected binary detached (F25), generating a
// config first when Syncthing has none yet.
func (a *App) startSyncthing(ctx context.Context, inst stinstall.Install) error {
	a.mu.Lock()
	if !a.starting.IsZero() && a.now().Sub(a.starting) < startDebounce {
		a.mu.Unlock()
		return nil
	}
	a.starting = a.now()
	a.mu.Unlock()

	generated := false
	if _, _, err := a.discover(ctx, inst.Bin); errors.Is(err, stclient.ErrConfigNotFound) {
		home, herr := syncthingHome()
		if herr != nil {
			a.note("Setup failed: "+herr.Error(), status.TintRed)
			return herr
		}
		a.note("Setting up Syncthing", status.TintAmber)
		if err := stinstall.Generate(ctx, inst.Bin, home); err != nil {
			a.note("Setup failed: "+err.Error(), status.TintRed)
			return err
		}
		generated = true
	}
	if err := a.startST(inst.Bin); err != nil {
		a.mu.Lock()
		a.starting = time.Time{}
		a.mu.Unlock()
		a.note("Start failed: "+err.Error(), status.TintRed)
		return err
	}
	a.note("Starting Syncthing", status.TintAmber)
	a.goRun(func() { a.afterStart(inst, generated) })
	return nil
}

// afterStart waits for the API of a Syncthing just started, then connects,
// and for a config SyncThing V2 generated applies the tailnet profile and
// sets up Syncthing's autostart when it owns the binary (§5.2 step 6).
func (a *App) afterStart(inst stinstall.Install, generated bool) {
	ctx, cancel := context.WithTimeout(a.ctx, apiWait)
	defer cancel()
	c, err := a.waitAPI(ctx, inst.Bin)
	if err != nil {
		applog.Printf("app: Syncthing did not answer after start: %v", err)
		a.connect(a.ctx)
		return
	}
	if generated {
		a.mu.Lock()
		a.generated = true
		a.mu.Unlock()
		if err := stinstall.ApplyProfile(ctx, c, stinstall.Tailnet); err != nil {
			applog.Printf("app: applying the tailnet profile: %v", err)
			a.note("Could not apply the tailnet profile: "+err.Error(), status.TintRed)
		} else {
			if err := a.prefs.Update(func(p *prefs.Prefs) error { p.ProfileChosen = prefs.ProfileTailnet; return nil }); err != nil {
				applog.Printf("app: saving prefs: %v", err)
			}
		}
	}
	if inst.Managed {
		if _, foreign := a.autoExisting(autostart.Syncthing); !foreign {
			if err := a.autoSet(autostart.Syncthing, true, inst.Bin); err != nil {
				applog.Printf("app: Syncthing autostart: %v", err)
			}
		}
	}
	a.connect(a.ctx)
}

// waitAPI polls until the Syncthing API answers and returns its client.
func (a *App) waitAPI(ctx context.Context, bin string) (*stclient.Client, error) {
	var last error
	for {
		ep, _, err := a.discover(ctx, bin)
		if err == nil {
			c := stclient.New(ep)
			var st struct {
				MyID string `json:"myID"`
			}
			if err = c.Get(ctx, "/rest/system/status", &st); err == nil {
				return c, nil
			}
		}
		last = err
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
		case <-t.C:
		}
	}
}

// bootstrap downloads the pinned Syncthing into the managed directory,
// generates its config, starts it, applies the tailnet profile and sets up
// its autostart (§5.2).
func (a *App) bootstrap(ctx context.Context) error {
	a.mu.Lock()
	if a.booting {
		a.mu.Unlock()
		return nil
	}
	a.booting = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.booting, a.bootMsg = false, ""
		a.mu.Unlock()
		a.publish()
	}()

	setMsg := func(msg string) {
		a.mu.Lock()
		changed := a.bootMsg != msg
		a.bootMsg = msg
		a.mu.Unlock()
		if changed {
			a.publish()
		}
	}
	fail := func(what string, err error) error {
		a.note(what+": "+err.Error(), status.TintRed)
		a.notify("Syncthing setup failed", what+". Run \"stv2 doctor\" for details.")
		return fmt.Errorf("%s: %w", what, err)
	}

	dir, err := stinstall.ManagedDir()
	if err != nil {
		return fail("Could not find the install folder", err)
	}
	v := brand.BootstrapSyncthing
	a.note("Downloading Syncthing "+v, status.TintBlue)
	setMsg("Downloading Syncthing " + v + "…")
	var mu sync.Mutex
	lastPct := -1
	bin, err := stinstall.Download(ctx, dir, func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := int(done * 100 / total)
		mu.Lock()
		step := pct != lastPct
		lastPct = pct
		mu.Unlock()
		if step {
			setMsg("Downloading Syncthing " + v + "… " + strconv.Itoa(pct) + "%")
		}
	})
	if err != nil {
		return fail("Syncthing download failed", err)
	}
	a.note("Syncthing "+v+" downloaded and verified", status.TintGreen)

	home, err := syncthingHome()
	if err != nil {
		return fail("Could not find Syncthing's home folder", err)
	}
	generated := false
	if _, err := os.Stat(filepath.Join(home, stclient.ConfigFile)); errors.Is(err, os.ErrNotExist) {
		setMsg("Setting up Syncthing…")
		if err := stinstall.Generate(ctx, bin, home); err != nil {
			return fail("Syncthing setup failed", err)
		}
		generated = true
	}
	inst := stinstall.Install{Bin: bin, Managed: true, Version: v}
	a.mu.Lock()
	a.setInstLocked(inst)
	a.mu.Unlock()

	setMsg("Starting Syncthing…")
	if err := a.startST(bin); err != nil {
		return fail("Syncthing did not start", err)
	}
	a.note("Starting Syncthing", status.TintAmber)
	a.mu.Lock()
	a.starting = a.now()
	a.mu.Unlock()
	a.afterStart(inst, generated)
	a.notify("Syncthing is ready", "Syncthing "+v+" was installed and started.")
	return nil
}

// syncthingHome is where a new Syncthing config is generated: $STHOMEDIR
// when set (Discover looks there first), otherwise Syncthing's default home.
func syncthingHome() (string, error) {
	if h := os.Getenv("STHOMEDIR"); filepath.IsAbs(h) {
		return h, nil
	}
	return stinstall.DefaultHome()
}

// setupFlow is the --setup (and first-run) behaviour: Syncthing is started,
// or downloaded and set up when it is missing.
func (a *App) setupFlow() {
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	// Give the first connect a moment, so a running Syncthing is not started twice.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		a.mu.Lock()
		decided := (a.sess != nil && a.haveBase) || a.downReason != ""
		a.mu.Unlock()
		if decided {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			decided = true
		case <-tick.C:
		}
		if decided {
			break
		}
	}
	a.mu.Lock()
	down := a.sess == nil || (a.haveBase && a.base.State == model.StateDown)
	a.mu.Unlock()
	if down {
		if err := a.ensureSyncthing(ctx); err != nil {
			applog.Printf("app: setup: %v", err)
		}
	}
}

// ---------- autostart ----------

func (a *App) autoEnabled(t autostart.Target) bool {
	if a.auto == nil {
		return false
	}
	on, err := a.auto.Enabled(t)
	if err != nil {
		applog.Printf("app: autostart %s: %v", t, err)
	}
	return on
}

// autoExisting reports a Syncthing login entry configured outside SyncThing
// V2. On macOS that includes a Syncthing run from inside another app's
// bundle (Syncthing.app, also installed by the Homebrew cask): that app
// starts it at login through its own login item, and a LaunchAgent of ours
// for the same binary would start a second Syncthing on the same home.
func (a *App) autoExisting(t autostart.Target) (string, bool) {
	if t == autostart.Syncthing && goos == "darwin" {
		if bin := a.stBin.Load(); bin != nil {
			if app := autostart.AppBundle(*bin); app != "" {
				return app + " manages Syncthing", true
			}
		}
	}
	if a.auto == nil {
		return "", false
	}
	return a.auto.Existing(t)
}

// setInstLocked records the Syncthing installation in use. a.mu must be held.
func (a *App) setInstLocked(i stinstall.Install) {
	a.inst, a.instFound = i, true
	bin := i.Bin
	a.stBin.Store(&bin)
}

func (a *App) autoLoaded(t autostart.Target) bool {
	return a.auto != nil && a.auto.Loaded(t)
}

func (a *App) autoSet(t autostart.Target, on bool, bin string) error {
	if a.auto == nil {
		return errors.New("autostart is not available on this system")
	}
	err := a.auto.Set(t, on, bin, nil)
	a.stMu.Lock()
	a.startupAt = time.Time{}
	a.stMu.Unlock()
	return err
}

// startupCached reports the autostart entries, re-reading them at most every
// startupCacheTTL. The engine calls it for every snapshot.
func (a *App) startupCached() model.Startup {
	a.stMu.Lock()
	defer a.stMu.Unlock()
	if !a.startupAt.IsZero() && a.now().Sub(a.startupAt) < startupCacheTTL {
		return a.startup
	}
	own := a.autoEnabled(autostart.Syncthing)
	_, foreign := a.autoExisting(autostart.Syncthing)
	a.startup = model.Startup{
		Syncthing:            own || foreign,
		Tray:                 a.autoEnabled(autostart.Tray),
		SyncthingManagedByUs: own,
	}
	a.startupAt = a.now()
	return a.startup
}

// trayExe is the executable the tray autostart entry starts: the installed
// copy when there is one, otherwise this executable.
func (a *App) trayExe() string {
	if r, err := a.roots(); err == nil {
		exe := install.InstalledExe(r)
		if fi, err := os.Stat(exe); err == nil && fi.Mode().IsRegular() {
			return exe
		}
	}
	return a.exe
}

// setAutostart handles the "Start ... at login" toggles (§5.3). Syncthing's
// entry is changed only when SyncThing V2 owns it or none exists.
func (a *App) setAutostart(target string, on bool) error {
	switch target {
	case "tray":
		exe := a.trayExe()
		if on && exe == "" {
			return errors.New("the " + brand.DisplayName + " executable was not found")
		}
		if on && goos == "darwin" && install.CheckLoginPath(exe) != nil {
			return userError("Move " + brand.DisplayName + " to Applications first, then turn this on.")
		}
		if err := a.autoSet(autostart.Tray, on, exe); err != nil {
			return err
		}
	case "syncthing":
		own := a.autoEnabled(autostart.Syncthing)
		if _, foreign := a.autoExisting(autostart.Syncthing); foreign && !own {
			return userError(autostart.ExternalText)
		}
		a.mu.Lock()
		bin, found := a.inst.Bin, a.instFound
		a.mu.Unlock()
		if on && (!found || bin == "") {
			return userError("Syncthing was not found on this computer")
		}
		// Removing our entry unloads the launchd job, which stops the
		// Syncthing it runs, but only when launchd has it loaded. A job
		// written this session for a Syncthing started detached is not.
		restart := !on && own && found && autostartOffStopsSyncthing && a.autoLoaded(autostart.Syncthing)
		if err := a.autoSet(autostart.Syncthing, on, bin); err != nil {
			return err
		}
		if restart {
			// Start Syncthing again unsupervised so syncing goes on; the
			// toggle only concerns the next login.
			if err := a.startST(bin); err != nil {
				return fmt.Errorf("restarting Syncthing after turning its login entry off: %w", err)
			}
			a.mu.Lock()
			a.starting = a.now()
			a.mu.Unlock()
		}
	default:
		return fmt.Errorf("unknown autostart target %q", target)
	}
	if s := a.session(); s != nil {
		s.engine.Refresh()
	} else {
		a.publish()
	}
	return nil
}
