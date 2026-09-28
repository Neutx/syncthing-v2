package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/status"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
	"github.com/Neutx/syncthing-v2/internal/ui"
	"github.com/Neutx/syncthing-v2/internal/update"
)

// The App is the dashboard's ui.Backend.

// Snapshots subscribes to decorated snapshots; the latest one is delivered
// first.
func (a *App) Snapshots() (<-chan model.Snapshot, func()) {
	ch := make(chan model.Snapshot, 1)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	if a.haveLast {
		ch <- a.last
	}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.mu.Unlock()
	}
}

// Backdrop returns the glass backdrop rendered by the last Show, or nil.
func (a *App) Backdrop() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backdrop
}

// actionArgs is the union of every action's arguments.
type actionArgs struct {
	ID         string   `json:"id"`
	Target     string   `json:"target"`
	On         bool     `json:"on"`
	DeviceID   string   `json:"deviceID"`
	DeviceIDs  []string `json:"deviceIDs"`
	FolderID   string   `json:"folderID"`
	FromDevice string   `json:"fromDevice"`
	Path       string   `json:"path"`
	Label      string   `json:"label"`
	Forever    bool     `json:"forever"`
	Yes        bool     `json:"yes"`
	Key        string   `json:"key"`
	Profile    string   `json:"profile"`
	Page       string   `json:"page"`
}

// settings is the result of get-settings, set-pref and set-profile.
type settings struct {
	Notifications   bool   `json:"notifications"`
	CheckForUpdates bool   `json:"checkForUpdates"`
	Profile         string `json:"profile"`
}

// Action runs a dashboard command (§3.9). "show" and "quit" come from a
// second instance or the installer through the bearer-authenticated
// /api/show and /api/quit.
func (a *App) Action(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var args actionArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments for %s", name)
		}
	}
	switch name {
	case "show":
		go a.Show()
		return nil, nil
	case "quit":
		// Let the response go out before the server stops.
		time.AfterFunc(200*time.Millisecond, a.Quit)
		return nil, nil
	case "hide":
		a.hide()
		return nil, nil

	case "rescan", "pause", "resume", "restart":
		return nil, a.engineCommand(ctx, name)
	case "start-syncthing":
		return nil, a.ensureSyncthing(ctx)
	case "open-folder":
		return a.openFolderByID(args.ID)
	case "open-webui":
		url := a.lastSnapshot().GUIURL
		if url == "" {
			return nil, errNoSyncthing
		}
		return nil, a.open(url)
	case "open-update":
		a.mu.Lock()
		u := a.upd.Latest.URL
		a.mu.Unlock()
		if u == "" {
			u = update.ReleasesPage
		}
		return nil, a.open(u)
	case "open-logs":
		dir, err := osutil.LogDir()
		if err == nil {
			err = osutil.EnsureDir(dir)
		}
		if err == nil {
			err = a.openFolder(dir)
		}
		return nil, err
	case "open-docs":
		u, ok := ui.DocsURL(args.Page)
		if !ok {
			return nil, fmt.Errorf("no documentation page %q", args.Page)
		}
		return nil, a.open(u)
	case "copy-diagnostics":
		return map[string]string{"text": a.diagnostics(ctx)}, nil
	case "set-autostart":
		return nil, a.setAutostart(args.Target, args.On)

	case "discover":
		return a.discoverAction(ctx)
	case "pair":
		return nil, a.pair(ctx, args.DeviceID)
	case "accept-device":
		return nil, a.acceptDevice(ctx, args.DeviceID)
	case "decline-device":
		return nil, a.declineDevice(ctx, args.DeviceID)
	case "share-folder":
		return nil, a.shareFolder(ctx, args)
	case "pick-folder":
		p, err := a.pickShareFolder()
		if err != nil {
			return nil, err
		}
		return map[string]string{"path": p}, nil
	case "accept-folder":
		return nil, a.acceptFolder(ctx, args)
	case "decline-folder":
		return nil, a.declineFolder(ctx, args)

	case "fix-gui-exposure":
		return nil, a.fixGUIExposure(ctx)
	case "dismiss-notice":
		return nil, a.dismissNotice(args.ID, args.Forever)
	case "migrate-legacy":
		return nil, a.migrateLegacy(args.Yes)
	case "install-tailscale":
		return nil, a.installTailscale()
	case "allow-firewall":
		return a.allowFirewall(ctx)

	case "get-settings":
		return a.settings(ctx), nil
	case "set-pref":
		if err := a.setPref(args.Key, args.On); err != nil {
			return nil, err
		}
		return a.settings(ctx), nil
	case "set-profile":
		if err := a.setProfile(ctx, args.Profile); err != nil {
			return nil, err
		}
		return a.settings(ctx), nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

// allowFirewall is the welcome view's "Allow through firewall" (§5.5): it
// adds the scoped inbound rule for the Syncthing in use, which on Windows
// asks for administrator approval (UAC). The result reports whether the rule
// was already there, in which case no prompt is shown.
func (a *App) allowFirewall(ctx context.Context) (map[string]bool, error) {
	a.mu.Lock()
	bin, found := a.inst.Bin, a.instFound
	a.mu.Unlock()
	if !found || bin == "" {
		return nil, userError("Syncthing was not found on this computer. Start Syncthing first.")
	}
	if has, err := a.hasFW(ctx); err == nil && has {
		return map[string]bool{"already": true}, nil
	}
	switch err := a.allowFW(ctx, bin); {
	case err == nil:
		a.note("Firewall rule added for Syncthing", status.TintGreen)
		return map[string]bool{"already": false}, nil
	case errors.Is(err, install.ErrElevationDeclined):
		return nil, userError("Administrator approval was not given, so no firewall rule was added")
	default:
		return nil, err
	}
}

// open opens a URL, noting a failure like the prototype (F28).
func (a *App) open(target string) error {
	if err := a.openURL(target); err != nil {
		a.note("Could not open: "+err.Error(), status.TintRed)
		return err
	}
	return nil
}

// engineCommand runs rescan, pause, resume or restart (F27); the engine notes
// the outcome itself.
func (a *App) engineCommand(ctx context.Context, name string) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	switch name {
	case "rescan":
		return s.engine.Rescan(ctx)
	case "pause":
		return s.engine.Pause(ctx)
	case "resume":
		return s.engine.Resume(ctx)
	default:
		return s.engine.Restart(ctx)
	}
}

// openFolderByID opens a Syncthing folder in the file manager (F24): the
// folder with id, or with an empty id the first folder whose path exists.
func (a *App) openFolderByID(id string) (any, error) {
	snap := a.lastSnapshot()
	for _, f := range snap.Folders {
		if id != "" && f.ID != id {
			continue
		}
		p, err := expandHome(f.Path)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			if err := a.openFolder(p); err != nil {
				a.note("Could not open: "+err.Error(), status.TintRed)
				return nil, err
			}
			return nil, nil
		}
		if id != "" {
			break
		}
	}
	err := userError("No folder path available")
	a.note(err.Error(), status.TintRed)
	return nil, err
}

// expandHome resolves Syncthing's "~" prefix in folder paths.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		h, err := osutil.HomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, p[1:]), nil
	}
	if p == "" {
		return "", errors.New("empty path")
	}
	return p, nil
}

// diagnostics is the redacted "Copy diagnostics" report (F17).
func (a *App) diagnostics(ctx context.Context) string {
	a.mu.Lock()
	inst, found := a.inst, a.instFound
	s := a.sess
	a.mu.Unlock()
	extra := map[string]string{
		"app":       brand.DisplayName + " " + brand.Version + " (" + brand.Commit() + ")",
		"os":        runtime.GOOS + "/" + runtime.GOARCH,
		"go":        runtime.Version(),
		"dashboard": map[bool]string{true: "browser", false: "glass"}[a.browserMode()],
	}
	if found {
		extra["syncthing"] = inst.Version
		extra["syncthing managed"] = fmt.Sprint(inst.Managed)
	} else {
		extra["syncthing"] = "not found"
	}
	tctx, cancel := context.WithTimeout(ctx, tailnet.StatusTimeout)
	defer cancel()
	if st, err := a.ts.Status(tctx); err == nil {
		extra["tailscale"] = st.BackendState
	} else if errors.Is(err, tailnet.ErrNotInstalled) {
		extra["tailscale"] = "not installed"
	} else {
		extra["tailscale"] = "unavailable"
	}
	if s != nil {
		if p, ok, err := stinstall.CurrentProfile(tctx, s.client); err == nil {
			extra["profile"] = "custom"
			if ok {
				extra["profile"] = p.String()
			}
		}
	}
	return status.Diagnostics(a.lastSnapshot(), extra)
}

// ---------- pairing ----------

// tailnetError turns pairing errors about Tailscale into the texts of §4.2.
func tailnetError(err error) error {
	switch {
	case errors.Is(err, tailnet.ErrNotInstalled):
		return ui.WithDocs(userError("Tailscale is not installed. Install it and sign in, then try again."), "tailscale-setup")
	case errors.Is(err, pairing.ErrTailscaleNotRunning):
		return ui.WithDocs(userError("Tailscale is not connected. Open Tailscale and sign in."), "tailscale-not-connected")
	}
	return err
}

func (a *App) discoverAction(ctx context.Context) ([]model.Candidate, error) {
	s := a.session()
	if s == nil {
		return nil, errNoSyncthing
	}
	cands, err := a.runDiscovery(ctx, s)
	if err != nil {
		return nil, tailnetError(err)
	}
	if cands == nil {
		cands = []model.Candidate{}
	}
	return cands, nil
}

func (a *App) pair(ctx context.Context, deviceID string) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	id, err := deviceid.Parse(deviceID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	c, ok := a.cands[id]
	a.mu.Unlock()
	if !ok {
		// The list may be from before a restart of the tray; look again.
		if _, err := a.runDiscovery(ctx, s); err != nil {
			return tailnetError(err)
		}
		a.mu.Lock()
		c, ok = a.cands[id]
		a.mu.Unlock()
		if !ok {
			return userError("This device is no longer offered. Refresh the list and try again.")
		}
	}
	if c.Status != pairing.StatusReady && c.Status != pairing.StatusPaired {
		return userError("This device cannot be paired right now")
	}
	if err := s.svc.Pair(ctx, c); err != nil {
		return err
	}
	a.mu.Lock()
	c.Status = pairing.StatusPaired
	a.cands[id] = c
	a.mu.Unlock()
	s.engine.Note("Pairing requested: "+c.NodeName, status.TintBlue)
	s.engine.Refresh()
	return nil
}

func (a *App) acceptDevice(ctx context.Context, deviceID string) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	// Only the device ID comes from the request; the verified request itself
	// is looked up in the watcher (never deserialised from the browser).
	pd, err := s.watcher.Accept(ctx, deviceID)
	if errors.Is(err, pairing.ErrNotVerified) {
		return userError("This pairing request is no longer pending")
	}
	if err != nil {
		return err
	}
	name := pd.Name
	if pd.Node != nil && pd.Node.NodeName != "" {
		name = pd.Node.NodeName
	}
	s.engine.Note("Paired with "+name, status.TintGreen)
	s.engine.Refresh()
	a.publish()
	return nil
}

func (a *App) declineDevice(ctx context.Context, deviceID string) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	if deviceID == "" {
		return errors.New("missing device")
	}
	if err := s.svc.Decline(ctx, deviceID); err != nil {
		return err
	}
	s.watcher.Resolve(deviceID)
	s.engine.Note("Pairing request declined", status.TintGrey)
	s.engine.Refresh()
	a.publish()
	return nil
}

// syncBase is ~/Sync, the default parent of new and accepted folders
// (§4.4); it is created when missing.
func syncBase() (string, error) {
	h, err := osutil.HomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(h, "Sync")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return base, nil
}

// pickShareFolder asks for a folder with the native picker, starting at ~/Sync.
func (a *App) pickShareFolder() (string, error) {
	base, err := syncBase()
	if err != nil {
		base = ""
	}
	return a.pickFolder("Choose a folder to share", base)
}

func (a *App) shareFolder(ctx context.Context, args actionArgs) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	if len(args.DeviceIDs) == 0 {
		return userError("Choose at least one device")
	}
	ids := make([]string, 0, len(args.DeviceIDs))
	for _, d := range args.DeviceIDs {
		id, err := deviceid.Parse(d)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	var id, label, path string
	if args.FolderID != "" {
		var found *model.Folder
		for _, f := range a.lastSnapshot().Folders {
			if f.ID == args.FolderID {
				found = &f
				break
			}
		}
		if found == nil {
			return userError("This folder is not configured in Syncthing")
		}
		id, label, path = found.ID, found.Label, found.Path
	} else {
		if args.Path == "" {
			return userError("Choose a folder")
		}
		if !filepath.IsAbs(args.Path) {
			return userError("The folder path must be absolute")
		}
		path = filepath.Clean(args.Path)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		label = args.Label
		if label == "" {
			label = filepath.Base(path)
		}
		fid, err := pairing.NewFolderID()
		if err != nil {
			return err
		}
		id = fid
	}
	if err := s.svc.ShareFolder(ctx, id, label, path, ids); err != nil {
		return err
	}
	s.engine.Note("Shared "+label, status.TintGreen)
	s.engine.Refresh()
	return nil
}

// findOffer returns the folder offer matching args.
func (a *App) findOffer(s *session, args actionArgs) (model.PendingFolder, bool) {
	for _, pf := range s.watcher.PendingFolders() {
		if pf.FolderID == args.FolderID && pf.FromDevice == args.FromDevice {
			return pf, true
		}
	}
	return model.PendingFolder{}, false
}

func (a *App) acceptFolder(ctx context.Context, args actionArgs) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	pf, ok := a.findOffer(s, args)
	if !ok {
		return userError("This folder is no longer offered")
	}
	path := args.Path
	if path == "" {
		base, err := syncBase()
		if err != nil {
			return err
		}
		if path, err = pairing.SafeFolderDir(base, pf.Label, pf.FolderID); err != nil {
			return err
		}
	} else if !filepath.IsAbs(path) {
		return userError("The folder path must be absolute")
	}
	// AcceptFolder creates the directory only when it creates the folder, so
	// a refused accept (the folder ID exists elsewhere) leaves nothing behind.
	if err := s.svc.AcceptFolder(ctx, pf, filepath.Clean(path)); err != nil {
		return err
	}
	s.engine.Note("Accepted "+folderLabel(pf), status.TintGreen)
	s.engine.Refresh()
	_ = s.watcher.Check(ctx)
	return nil
}

func (a *App) declineFolder(ctx context.Context, args actionArgs) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	pf, ok := a.findOffer(s, args)
	if !ok {
		return userError("This folder is no longer offered")
	}
	if err := s.svc.DeclineFolder(ctx, pf); err != nil {
		return err
	}
	s.engine.Note("Declined "+folderLabel(pf), status.TintGrey)
	s.engine.Refresh()
	_ = s.watcher.Check(ctx)
	return nil
}

// ---------- notices ----------

// fixGUIExposure restricts Syncthing's GUI to this computer (§5.4).
func (a *App) fixGUIExposure(ctx context.Context) error {
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	a.mu.Lock()
	port := a.guiPort
	a.mu.Unlock()
	if port <= 0 {
		port = 8384
	}
	body := map[string]any{"address": fmt.Sprintf("127.0.0.1:%d", port), "insecureAdminAccess": false}
	if err := s.client.Send(ctx, http.MethodPatch, "/rest/config/gui", body); err != nil {
		return err
	}
	a.mu.Lock()
	a.guiExposed = false
	a.mu.Unlock()
	s.engine.Note("Syncthing control panel restricted to this computer", status.TintGreen)
	a.publish()
	return nil
}

func (a *App) dismissNotice(id string, forever bool) error {
	switch id {
	case NoticeGUIExposed, NoticeTailscaleMissing, NoticeUpdate:
	case NoticeLegacyTray:
		return a.migrateLegacy(false)
	default:
		return fmt.Errorf("unknown notice %q", id)
	}
	a.mu.Lock()
	a.hidden[id] = true
	a.mu.Unlock()
	if forever && id != NoticeUpdate {
		if err := a.prefs.DismissNotice(id); err != nil {
			return err
		}
	}
	a.publish()
	return nil
}

// migrateLegacy answers the legacy-tray question (§6.5): yes stops the old
// tray and disables its Startup shortcut; either answer is final.
func (a *App) migrateLegacy(yes bool) error {
	var err error
	if yes {
		r, rerr := a.roots()
		if rerr != nil {
			return rerr
		}
		if err = install.MigrateLegacy(r, install.DetectLegacy(r)); err == nil {
			a.note("The older Syncthing tray was replaced", status.TintGreen)
		}
	}
	if perr := a.prefs.Update(func(p *prefs.Prefs) error { p.LegacyAsked = true; return nil }); perr != nil && err == nil {
		err = perr
	}
	a.mu.Lock()
	a.legacy = false
	a.mu.Unlock()
	a.publish()
	return err
}

// installTailscale runs the Tailscale installer through winget, in a visible
// console, only on the user's click (§4.2). The tray has no console of its
// own, so Windows gives winget a new, visible one.
func (a *App) installTailscale() error {
	if goos != "windows" {
		return userError("Install Tailscale from https://tailscale.com/download and sign in")
	}
	winget, err := exec.LookPath("winget")
	if err != nil {
		return errors.New("winget is not available. Install Tailscale from https://tailscale.com/download")
	}
	cmd := exec.Command(winget, "install", "-e", "--id", "Tailscale.Tailscale")
	if err := cmd.Start(); err != nil {
		return err
	}
	a.note("Installing Tailscale", status.TintBlue)
	a.goRun(func() {
		err := cmd.Wait()
		if err != nil {
			applog.Printf("app: winget install Tailscale: %v", err)
			a.note("Tailscale installation did not finish", status.TintRed)
		} else {
			a.note("Tailscale installed. Sign in to Tailscale to pair devices.", status.TintGreen)
		}
		a.checkTailscale()
	})
	return nil
}

// ---------- settings ----------

func (a *App) settings(ctx context.Context) settings {
	p := a.prefs.Get()
	out := settings{Notifications: p.Notifications, CheckForUpdates: p.CheckForUpdates, Profile: p.ProfileChosen}
	if s := a.session(); s != nil {
		if prof, ok, err := stinstall.CurrentProfile(ctx, s.client); err == nil {
			out.Profile = ""
			if ok {
				out.Profile = prof.String()
			}
		}
	}
	return out
}

func (a *App) setPref(key string, on bool) error {
	var fn func(*prefs.Prefs)
	switch key {
	case "notifications":
		fn = func(p *prefs.Prefs) { p.Notifications = on }
	case "checkForUpdates":
		fn = func(p *prefs.Prefs) { p.CheckForUpdates = on }
	default:
		return fmt.Errorf("unknown preference %q", key)
	}
	if err := a.prefs.Update(func(p *prefs.Prefs) error { fn(p); return nil }); err != nil {
		return err
	}
	if key == "checkForUpdates" && on && a.checker != nil {
		a.goRun(func() {
			ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
			defer cancel()
			if r, err := a.checker.Check(ctx); err == nil {
				a.onUpdate(r)
			}
		})
	}
	a.publish()
	return nil
}

// setProfile applies a transport profile to the current Syncthing (§4.5);
// the dashboard asked for confirmation first.
func (a *App) setProfile(ctx context.Context, name string) error {
	p, err := stinstall.ParseProfile(name)
	if err != nil {
		return err
	}
	s := a.session()
	if s == nil {
		return errNoSyncthing
	}
	if err := stinstall.ApplyProfile(ctx, s.client, p); err != nil {
		return err
	}
	if err := a.prefs.Update(func(pp *prefs.Prefs) error { pp.ProfileChosen = p.String(); return nil }); err != nil {
		applog.Printf("app: saving prefs: %v", err)
	}
	s.engine.Note("Transport profile set to "+p.String(), status.TintBlue)
	return nil
}
