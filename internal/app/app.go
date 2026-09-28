// Package app wires SyncThing V2 together: it finds Syncthing, runs the status
// engine, and feeds its snapshots to the tray icon, the notifications and the
// dashboard server; it maps every dashboard and tray command to the engine,
// the pairing service, autostart, the installer helpers and the desktop shell.
//
// Syncthing always runs as its own process. Exiting the tray stops the
// engine, the dashboard server and the dashboard host, and leaves Syncthing
// running (F7).
package app

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"runtime"

	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/glass"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/notify"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/picker"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/single"
	"github.com/Neutx/syncthing-v2/internal/status"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
	"github.com/Neutx/syncthing-v2/internal/tray"
	"github.com/Neutx/syncthing-v2/internal/ui"
	"github.com/Neutx/syncthing-v2/internal/uihost"
	"github.com/Neutx/syncthing-v2/internal/update"
)

// Notice IDs carried in model.Snapshot.Notices, plus "update" for the
// update banner, which dismiss-notice also accepts.
const (
	NoticeGUIExposed       = "gui-exposed"
	NoticeLegacyTray       = "legacy-tray"
	NoticeTailscaleMissing = "tailscale-missing"
	NoticeUpdate           = "update"
)

// Timing.
const (
	// reconnectEvery is how often the app looks for Syncthing again while it
	// has none, or while Syncthing is down or rejects the API key (the config
	// may have been regenerated with a new port or key).
	reconnectEvery = 30 * time.Second
	// startupCacheTTL bounds how stale the autostart state in snapshots is.
	startupCacheTTL = 10 * time.Second
	// captureSettle is how long the tray waits after hiding a visible popup
	// before it photographs the desktop behind it (§6.4).
	captureSettle = 45 * time.Millisecond
)

// Options select how the tray starts.
type Options struct {
	// Background is the autostart mode: tray only, no dashboard.
	Background bool
	// Setup opens the dashboard and runs the setup flow: Syncthing is
	// started, or downloaded and set up when it is missing. A first run
	// (no prefs.json yet) implies Setup unless Background is set.
	Setup bool
	// Lock is the single-instance lock, already held (see Acquire).
	Lock *single.Lock
	// Message shows a modal information box (the About box). Nil opens the
	// dashboard instead, whose Settings view carries the About section.
	Message func(title, text string)
}

// autostarter is the part of *autostart.Manager the app uses.
type autostarter interface {
	Enabled(t autostart.Target) (bool, error)
	Existing(t autostart.Target) (string, bool)
	Set(t autostart.Target, on bool, bin string, args []string) error
	Loaded(t autostart.Target) bool
}

// App is the running tray application.
type App struct {
	opts    Options
	prefs   *prefs.Store
	dataDir string
	exe     string // this executable

	// Platform seams (production values from newApp; tests replace them).
	tray        tray.Tray
	notifyFn    func(title, body string, onClick func())
	openURL     func(string) error
	openFolder  func(string) error
	pickFolder  func(title, initial string) (string, error)
	place       func(image.Point) (image.Rectangle, float64)
	capture     func(image.Rectangle) (*image.RGBA, error)
	render      func(*image.RGBA, float64) *image.RGBA
	auto        autostarter
	ts          tailnet.Source
	probe       func(context.Context, netip.AddrPort) (string, error) // nil: pairing.Probe
	locateTS    func() (string, error)
	detect      func(context.Context) (stinstall.Install, error)
	discover    func(ctx context.Context, bin string) (stclient.Endpoint, string, error)
	readConfig  func(path string) (stclient.Endpoint, stclient.GUIConfig, error)
	roots       func() (install.Roots, error)
	hasSNI      func() bool
	sniWait     time.Duration // how long a missing tray host is waited for (UI002)
	sniPoll     time.Duration
	newEngine   func(c *stclient.Client, startup func() model.Startup) engine
	startST     func(bin string) error
	allowFW     func(ctx context.Context, bin string) error
	hasFW       func(ctx context.Context) (bool, error)
	now         func() time.Time
	reconnectIn time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	server  *ui.Server
	checker *update.Checker

	mu          sync.Mutex
	host        uihost.Host
	sess        *session
	inst        stinstall.Install
	instFound   bool
	stBin       atomic.Pointer[string] // inst.Bin, readable without mu (see autoExisting)
	base        model.Snapshot         // latest engine snapshot of sess
	haveBase    bool
	downReason  string // Detail of the synthetic Down snapshot (no session)
	notes       []model.ActivityItem
	bootMsg     string
	booting     bool
	starting    time.Time // last "start Syncthing" (debounces double starts)
	generated   bool      // the config in use was generated by SyncThing V2
	guiExposed  bool
	guiPort     int
	legacy      bool
	tsMissing   bool
	hidden      map[string]bool // notices hidden until the next launch
	upd         update.Result
	cands       map[string]model.Candidate
	backdrop    []byte
	subs        map[chan model.Snapshot]struct{}
	last        model.Snapshot
	haveLast    bool
	prevState   model.State
	havePrev    bool
	pendingSig  string
	connected   map[string]bool
	widening    map[string]bool
	secNotified bool
	discovered  bool

	stMu      sync.Mutex
	startup   model.Startup
	startupAt time.Time

	pubMu      sync.Mutex // serialises publish, so tray and subscribers see snapshots in order
	trayIcon   [2]int
	trayIconOK bool
	trayTip    string
	trayMenu   []tray.MenuItem

	// connMu serialises connect: connectLoop and afterStart may both call it,
	// and only one of them may attach a session for a new endpoint.
	connMu   sync.Mutex
	showMu   sync.Mutex
	quitOnce sync.Once
	wg       sync.WaitGroup
}

// engine is the part of *status.Engine the app uses.
type engine interface {
	Run(ctx context.Context) <-chan model.Snapshot
	Refresh()
	Note(text, tint string)
	Rescan(ctx context.Context) error
	Restart(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

// session is one connection to a Syncthing instance: its client, the engine
// polling it and the pairing service and watcher working on it.
type session struct {
	ep         stclient.Endpoint
	configPath string
	gui        stclient.GUIConfig
	client     *stclient.Client
	engine     engine
	svc        *pairing.Service
	watcher    *pairing.Watcher
	cancel     context.CancelFunc
}

// newApp returns an App with the production platform seams.
func newApp(opts Options, p *prefs.Store, dataDir string) *App {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	a := &App{
		opts:        opts,
		prefs:       p,
		dataDir:     dataDir,
		exe:         exe,
		notifyFn:    notify.Show,
		openURL:     osutil.OpenURL,
		openFolder:  osutil.OpenFolder,
		pickFolder:  picker.Folder,
		place:       uihost.Place,
		capture:     glass.Capture,
		render:      glass.Render,
		ts:          Tailnet(),
		locateTS:    tailnet.Locate,
		detect:      stinstall.Detect,
		discover:    stclient.Discover,
		readConfig:  stclient.ReadConfig,
		roots:       install.DefaultRoots,
		hasSNI:      notify.HasStatusNotifierWatcher,
		sniWait:     30 * time.Second,
		sniPoll:     time.Second,
		startST:     func(bin string) error { return stinstall.Start(bin) },
		allowFW:     install.AllowFirewall,
		hasFW:       install.HasFirewallRule,
		now:         time.Now,
		reconnectIn: reconnectEvery,
		hidden:      map[string]bool{},
		cands:       map[string]model.Candidate{},
		subs:        map[chan model.Snapshot]struct{}{},
		connected:   map[string]bool{},
		widening:    map[string]bool{},
	}
	a.newEngine = func(c *stclient.Client, startup func() model.Startup) engine {
		return status.NewEngine(c, startup)
	}
	if m, err := autostart.Default(); err == nil {
		a.auto = m
	} else {
		applog.Printf("app: autostart unavailable: %v", err)
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	return a
}

// Dirs returns SyncThing V2's data and log directories, creating both.
func Dirs() (dataDir, logDir string, err error) {
	if dataDir, err = osutil.AppDataDir(); err != nil {
		return "", "", err
	}
	if logDir, err = osutil.LogDir(); err != nil {
		return "", "", err
	}
	if err = osutil.EnsureDir(dataDir); err != nil {
		return "", "", err
	}
	if err = osutil.EnsureDir(logDir); err != nil {
		return "", "", err
	}
	return dataDir, logDir, nil
}

// Acquire takes the single-instance lock. When another tray already runs it
// returns single.ErrAlreadyRunning.
func Acquire() (*single.Lock, error) {
	dataDir, _, err := Dirs()
	if err != nil {
		return nil, err
	}
	return single.Acquire(brand.MutexName, dataDir)
}

// SignalRunning asks the running tray to act ("show" or "quit"). The
// running instance may still be starting, so a missing or stale
// instance.json is retried for a few seconds.
func SignalRunning(ctx context.Context, action string) error {
	dataDir, err := osutil.AppDataDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		err = single.Signal(ctx, dataDir, action)
		if err == nil {
			return nil
		}
		t := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// Run runs the tray until Exit (or /api/quit, or a termination signal).
// It must be called on the main goroutine with the OS thread locked, since
// the tray owns it. opts.Lock must be held; Run releases it on return.
func Run(opts Options) error {
	defer func() {
		if opts.Lock != nil {
			_ = opts.Lock.Release()
		}
	}()
	dataDir, logDir, err := Dirs()
	if err != nil {
		return err
	}
	if err := applog.Init(logDir); err != nil {
		fmt.Fprintf(os.Stderr, "%s: log: %v\n", brand.BinaryName, err)
	}
	defer applog.Close()
	applog.Printf("app: %s %s (%s) starting, pid %d", brand.DisplayName, brand.Version, brand.Commit(), os.Getpid())

	p, err := prefs.Open(dataDir)
	if err != nil {
		if p == nil {
			return fmt.Errorf("prefs: %w", err)
		}
		applog.Printf("app: %v (defaults in use)", err)
	}
	firstRun := !p.Existed()
	if firstRun {
		if err := p.Save(); err != nil {
			applog.Printf("app: saving prefs: %v", err)
		}
	}
	if firstRun && !opts.Background {
		opts.Setup = true
	}

	a := newApp(opts, p, dataDir)
	a.tray = tray.New()
	notify.Use(a.tray)
	if r, err := a.roots(); err == nil {
		if err := install.CleanupOld(r); err != nil {
			applog.Printf("app: removing the previous executable: %v", err)
		}
	}

	srv, err := ui.NewServer(a)
	if err != nil {
		return err
	}
	a.server = srv
	if err := single.WriteInstance(dataDir, single.Instance{Port: srv.Port(), PID: os.Getpid(), Token: srv.ControlToken()}); err != nil {
		applog.Printf("app: writing %s: %v", single.InstanceFile, err)
	}
	a.startHost()
	a.checker = update.New(p)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			applog.Printf("app: termination signal")
			a.Quit()
		case <-a.ctx.Done():
		}
	}()

	// The dashboard gets a "Connecting" snapshot at once; the tray keeps its
	// neutral icon and "starting" tooltip until the first look for Syncthing
	// has finished (see decidedLocked).
	a.publish()
	a.tray.Run(func() { a.start() }, func() { go a.Show() }, a.onMenu)

	// Exit (F7): stop the engine, the server and the host. Syncthing keeps running.
	signal.Stop(sig)
	a.shutdown()
	if err := single.RemoveInstance(dataDir, os.Getpid()); err != nil {
		applog.Printf("app: removing %s: %v", single.InstanceFile, err)
	}
	applog.Printf("app: exited")
	return nil
}

// start runs once the tray accepts updates.
func (a *App) start() {
	a.checkLegacy()
	a.checkTailscale()
	a.goRun(func() { a.connectLoop() })
	a.goRun(func() {
		a.checker.Run(a.ctx, a.onUpdate)
	})
	a.openOnStart()
	if a.opts.Setup {
		a.goRun(func() { a.setupFlow() })
	}
}

// openOnStart opens the dashboard as the tray starts: not in the autostart
// mode unless there is no tray icon to click (UI002), and on the welcome view
// for --setup and the first run (§6.1, §6.2).
//
// At login the tray often starts before the panel or the GNOME AppIndicator
// extension has claimed the StatusNotifierWatcher name, and the icon appears
// once it does. So a missing watcher is only reported after waiting up to
// sniWait for it; in the autostart mode the dashboard also waits.
func (a *App) openOnStart() {
	view := ""
	if a.opts.Setup {
		view = ViewWelcome
	}
	if a.hasSNI() {
		if !a.opts.Background {
			go a.ShowView(view)
		}
		return
	}
	if !a.opts.Background {
		go a.ShowView(view) // the dashboard opens anyway; only the notice waits
	}
	a.goRun(func() {
		if a.waitSNI() {
			return
		}
		// UI002: no StatusNotifierItem host, so the icon is invisible.
		applog.Printf("app: no StatusNotifierWatcher on the session bus after %v (UI002)", a.sniWait)
		a.notifyFn("No tray icon available",
			"Your desktop shows no tray icons. "+brand.DisplayName+" opens its dashboard in the browser instead. Run \"stv2 doctor\" (UI002) for help.", nil)
		if a.opts.Background {
			a.ShowView(view)
		}
	})
}

// waitSNI polls for a StatusNotifierWatcher every sniPoll for up to sniWait.
// It reports whether one appeared; false also when the app is quitting.
func (a *App) waitSNI() bool {
	deadline := time.NewTimer(a.sniWait)
	defer deadline.Stop()
	tick := time.NewTicker(a.sniPoll)
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return true // quitting: report nothing, open nothing
		case <-deadline.C:
			return a.hasSNI()
		case <-tick.C:
			if a.hasSNI() {
				applog.Printf("app: StatusNotifierWatcher appeared")
				return true
			}
		}
	}
}

func (a *App) goRun(fn func()) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		fn()
	}()
}

// Quit makes Run return. It is safe to call more than once and from any
// goroutine.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		applog.Printf("app: exit requested")
		a.cancel()
		if a.tray != nil {
			a.tray.Quit()
		}
	})
}

// shutdown stops everything the tray started, leaving Syncthing running.
func (a *App) shutdown() {
	a.cancel()
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	h := a.host
	a.host = nil
	a.mu.Unlock()
	if s != nil {
		s.cancel()
	}
	if h != nil {
		if err := h.Close(); err != nil {
			applog.Printf("app: closing the dashboard host: %v", err)
		}
	}
	if a.server != nil {
		_ = a.server.Close()
	}
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		applog.Printf("app: background work still running at exit")
	}
}

// ---------- snapshots ----------

// onEngineSnapshot takes a snapshot from the engine of s.
func (a *App) onEngineSnapshot(s *session, snap model.Snapshot) {
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	a.base, a.haveBase = snap, true
	sig := pendingSignature(snap)
	kick := sig != a.pendingSig
	a.pendingSig = sig
	var widen []string
	now := map[string]bool{}
	for _, p := range snap.Peers {
		if p.Connected {
			now[p.ID] = true
			if !a.connected[p.ID] && !a.widening[p.ID] {
				widen = append(widen, p.ID)
				a.widening[p.ID] = true
			}
		}
	}
	a.connected = now
	a.mu.Unlock()

	if kick && s.watcher != nil {
		s.watcher.Kick()
	}
	for _, id := range widen {
		a.goRun(func() { a.maybeWiden(s, id) })
	}
	a.publish()
}

// pendingSignature identifies Syncthing's raw pending lists, so a change
// (PendingDevicesChanged / PendingFoldersChanged, seen by the engine) wakes
// the pairing watcher at once instead of at its next 30 s poll.
func pendingSignature(s model.Snapshot) string {
	var b strings.Builder
	for _, p := range s.Pending {
		b.WriteString(p.DeviceID)
		b.WriteByte('@')
		b.WriteString(p.Addr.String())
		b.WriteByte(';')
	}
	b.WriteByte('|')
	for _, f := range s.PendingFolders {
		b.WriteString(f.FolderID)
		b.WriteByte('@')
		b.WriteString(f.FromDevice)
		b.WriteByte(';')
	}
	return b.String()
}

// publish builds the current snapshot and hands it to the dashboard
// subscribers and the tray, and raises the state-change notification (F23).
func (a *App) publish() {
	a.pubMu.Lock()
	defer a.pubMu.Unlock()

	a.mu.Lock()
	s := a.snapshotLocked()
	a.last, a.haveLast = s, true
	subs := make([]chan model.Snapshot, 0, len(a.subs))
	for ch := range a.subs {
		subs = append(subs, ch)
	}
	// Transitions count only between engine snapshots of one session, so
	// starting the tray (or reconnecting) never raises a notification for
	// the state that was already there.
	var prev model.State
	havePrev := false
	if a.sess != nil && a.haveBase {
		prev, havePrev = a.prevState, a.havePrev
		a.prevState, a.havePrev = s.State, true
	}
	decided := a.decidedLocked()
	a.mu.Unlock()

	for _, ch := range subs {
		offer(ch, s)
	}
	if decided {
		a.updateTray(s)
	}
	if havePrev {
		if title, body, ok := status.NoticeFor(prev, s.State, s); ok {
			a.notify(title, body)
		}
	}
}

// offer delivers s without blocking, replacing an unread older snapshot.
func offer(ch chan model.Snapshot, s model.Snapshot) {
	select {
	case ch <- s:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- s:
	default:
	}
}

// snapshotLocked returns the decorated snapshot: the engine's (or a
// synthetic Down snapshot when there is no Syncthing to talk to), with the
// verified pairing prompts, folder offers, one-time notices and the update.
func (a *App) snapshotLocked() model.Snapshot {
	var s model.Snapshot
	if a.sess != nil && a.haveBase {
		s = a.base
	} else {
		s = a.downSnapshotLocked()
	}
	// The engine lists every pending request Syncthing holds; the dashboard
	// only offers the ones the watcher verified over Tailscale (§4.3).
	s.Pending, s.PendingFolders = nil, nil
	if a.sess != nil && a.haveBase && a.sess.watcher != nil {
		s.Pending = a.sess.watcher.Pending()
		s.PendingFolders = a.sess.watcher.PendingFolders()
	}
	s.Notices = a.noticesLocked()
	p := a.prefs.Get()
	s.UpdateAvailable = ""
	if p.CheckForUpdates && a.upd.Available && !a.hidden[NoticeUpdate] {
		s.UpdateAvailable = a.upd.Latest.Version
	}
	return s
}

// decidedLocked reports whether the state of Syncthing is known: an engine
// snapshot arrived, the last look for Syncthing found none (or no config),
// or Syncthing is being set up. Before that (at launch, and briefly while a
// new session attaches) the tray keeps what it shows, so a Down state that is
// only a guess never offers "Start Syncthing".
func (a *App) decidedLocked() bool {
	return (a.sess != nil && a.haveBase) || a.downReason != "" || a.bootMsg != ""
}

// downSnapshotLocked is the snapshot shown while there is no engine: Syncthing
// is missing, not set up yet, or being downloaded. While the state is not yet
// known (decidedLocked) it says so instead of claiming Syncthing is down.
func (a *App) downSnapshotLocked() model.Snapshot {
	s := model.Snapshot{
		State:     model.StateDown,
		Label:     status.Label(model.StateDown),
		Headline:  status.Headline(model.StateDown),
		Subline:   "Start Syncthing to resume syncing.",
		Detail:    a.downReason,
		PeerLine:  "Syncthing not running",
		FilesLine: "-",
		SizeLine:  "Service offline",
		Pct:       100,
		At:        a.now(),
	}
	if a.bootMsg != "" {
		s.Subline, s.Detail = a.bootMsg, a.bootMsg
	}
	if !a.decidedLocked() {
		s.Connecting = true
		s.Headline = "Connecting"
		s.Subline, s.Detail = lookingForSyncthing, lookingForSyncthing
		s.PeerLine = lookingForSyncthing
		s.SizeLine = "-"
	}
	s.Activity = append([]model.ActivityItem(nil), a.notes...)
	s.Startup = a.startupCached()
	return s
}

// lookingForSyncthing is the Detail of the snapshot before the first look
// for Syncthing has finished.
const lookingForSyncthing = "Looking for Syncthing…"

// noticesLocked lists the one-time notices to show.
func (a *App) noticesLocked() []string {
	p := a.prefs.Get()
	var out []string
	if a.guiExposed && !a.hidden[NoticeGUIExposed] && !p.NoticeDismissed(NoticeGUIExposed) {
		out = append(out, NoticeGUIExposed)
	}
	if a.legacy && !p.LegacyAsked {
		out = append(out, NoticeLegacyTray)
	}
	if a.tsMissing && !a.hidden[NoticeTailscaleMissing] && !p.NoticeDismissed(NoticeTailscaleMissing) {
		out = append(out, NoticeTailscaleMissing)
	}
	return out
}

// lastSnapshot returns the latest published snapshot.
func (a *App) lastSnapshot() model.Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// updateTray pushes s to the tray, skipping unchanged parts.
func (a *App) updateTray(s model.Snapshot) {
	if a.tray == nil {
		return
	}
	icon := [2]int{int(s.State), s.Pct}
	if !a.trayIconOK || icon != a.trayIcon {
		a.tray.SetIcon(s.State, s.Pct)
		a.trayIcon, a.trayIconOK = icon, true
	}
	if tip := tray.Tooltip(s); tip != a.trayTip {
		a.tray.SetTooltip(tip)
		a.trayTip = tip
	}
	if m := tray.BuildMenu(s); !reflect.DeepEqual(m, a.trayMenu) {
		a.tray.SetMenu(m)
		a.trayMenu = m
	}
}

// ---------- notes and notifications ----------

// note adds a Recent Activity line: through the engine when there is one,
// otherwise to the synthetic snapshot.
func (a *App) note(text, tint string) {
	a.mu.Lock()
	s := a.sess
	if s == nil {
		a.notes = append([]model.ActivityItem{{At: a.now(), Text: text, Tint: tint}}, a.notes...)
		if len(a.notes) > status.MaxActivity {
			a.notes = a.notes[:status.MaxActivity]
		}
	}
	a.mu.Unlock()
	if s != nil {
		s.engine.Note(text, tint)
		return
	}
	a.publish()
}

// notify shows a notification that opens the dashboard when clicked, unless
// the user turned notifications off in Settings.
func (a *App) notify(title, body string) { a.notifyView(title, body, "") }

// notifyView is notify whose click opens the dashboard on view.
func (a *App) notifyView(title, body, view string) {
	if !a.prefs.Get().Notifications {
		return
	}
	a.notifyFn(title, body, func() { go a.ShowView(view) })
}

// ---------- tray menu ----------

func (a *App) onMenu(id string) {
	// Menu callbacks run on the tray thread (Windows) or a systray goroutine;
	// commands may take seconds, so they run elsewhere.
	go a.handleMenu(id)
}

func (a *App) handleMenu(id string) {
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	var err error
	switch id {
	case tray.IDOpenStatus:
		a.Show()
	case tray.IDPair:
		a.ShowView(ViewPair)
	case tray.IDOpenWebUI:
		_, err = a.Action(ctx, "open-webui", nil)
	case tray.IDRescan, tray.IDPause, tray.IDResume, tray.IDRestart, tray.IDStartSyncthing:
		// The engine already notes the outcome of its commands.
		_, err = a.Action(ctx, id, nil)
	case tray.IDAutostartTray:
		err = a.setAutostart("tray", !a.lastSnapshot().Startup.Tray)
	case tray.IDAutostartSyncthing:
		err = a.setAutostart("syncthing", !a.lastSnapshot().Startup.Syncthing)
	case tray.IDUpdate:
		_, err = a.Action(ctx, "open-update", nil)
	case tray.IDAbout:
		a.about()
	case tray.IDExit:
		a.Quit()
	default:
		if fid, ok := tray.FolderID(id); ok {
			_, err = a.openFolderByID(fid)
		} else {
			applog.Printf("app: unknown menu item %q", id)
		}
	}
	if err != nil {
		applog.Printf("app: menu %s: %v", id, err)
		a.menuFailed(id, err)
	}
}

// menuFailed reports a failed tray command. The engine commands, the open
// actions, the folder items and Start Syncthing already note their own
// failures in Recent Activity, so only what they leave unnoted is noted here.
// Start and the login toggles also show a notification, since the dashboard
// that holds the note is usually closed when the tray menu is used.
func (a *App) menuFailed(id string, err error) {
	switch id {
	case tray.IDAutostartTray, tray.IDAutostartSyncthing:
		a.note(err.Error(), status.TintRed)
		a.notify(brand.DisplayName, "Startup toggle failed: "+err.Error())
	case tray.IDStartSyncthing:
		a.notify("Syncthing", "Start failed: "+err.Error())
	default:
		if errors.Is(err, errNoSyncthing) { // refused before anything was noted
			a.note(err.Error(), status.TintRed)
		}
	}
}

// about shows the version and the disclaimer.
func (a *App) about() {
	if a.opts.Message == nil {
		a.ShowView(ViewSettings)
		return
	}
	a.opts.Message("About "+brand.DisplayName, AboutText())
}

// AboutText is the About box: name, version, commit, repository and the
// disclaimer (§0).
func AboutText() string {
	return fmt.Sprintf("%s %s (%s)\n%s\n\n%s", brand.DisplayName, brand.Version, brand.Commit(), brand.RepoURL, brand.Disclaimer)
}

// ---------- update check ----------

func (a *App) onUpdate(r update.Result) {
	a.mu.Lock()
	a.upd = r
	a.mu.Unlock()
	if r.Available && r.Notify && a.prefs.Get().Notifications {
		v := r.Latest.Version
		a.notifyFn("Update available", brand.DisplayName+" "+v+" is available.", func() { go a.Show() })
		if a.checker != nil {
			if err := a.checker.MarkNotified(v); err != nil {
				applog.Printf("app: recording the update notice: %v", err)
			}
		}
	}
	a.publish()
}

// ---------- widening after the first connection (§4.3 step 5) ----------

// maybeWiden adds "dynamic" to a device SyncThing V2 paired, on its first
// connection. A device counts as paired by SyncThing V2 when its addresses
// are exactly tailnet tcp:// and quic:// addresses on the sync port, which is
// what Pair and Accept write; devices configured any other way are left alone.
func (a *App) maybeWiden(s *session, id string) {
	defer func() {
		a.mu.Lock()
		delete(a.widening, id)
		a.mu.Unlock()
	}()
	if a.prefs.Get().IsWidened(id) {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	var dev struct {
		Addresses []string `json:"addresses"`
	}
	if err := s.client.Get(ctx, "/rest/config/devices/"+url.PathEscape(id), &dev); err != nil {
		applog.Printf("app: widen %s: %v", deviceid.Short(id), err)
		return
	}
	if !pairedByUs(dev.Addresses, tailnet.Prefixes) {
		return
	}
	if err := s.svc.Widen(ctx, id); err != nil {
		applog.Printf("app: widen %s: %v", deviceid.Short(id), err)
		return
	}
	applog.Printf("app: widened the addresses of %s", deviceid.Short(id))
}

// pairedByUs reports whether addrs are only tailnet tcp:// and quic://
// addresses on the sync port (the shape Pair and Accept write).
func pairedByUs(addrs []string, prefixes []netip.Prefix) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		scheme, rest, ok := strings.Cut(a, "://")
		if !ok || (scheme != "tcp" && scheme != "quic") {
			return false
		}
		ap, err := netip.ParseAddrPort(rest)
		if err != nil || ap.Port() != pairing.SyncPort || !inPrefixes(ap.Addr().Unmap(), prefixes) {
			return false
		}
	}
	return true
}

func inPrefixes(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// userError is a message shown to the user as it is, in a dashboard toast
// or an Activity note, so it is written as a sentence.
type userError string

func (e userError) Error() string { return string(e) }

// errNoSyncthing is returned by commands that need Syncthing when there is
// none to talk to.
const errNoSyncthing = userError("Syncthing is not running")

// goos is runtime.GOOS; a variable so tests can exercise per-OS branches.
var goos = runtime.GOOS
