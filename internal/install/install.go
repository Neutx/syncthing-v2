// Package install installs, upgrades and uninstalls SyncThing V2 for the
// current user (spec §6), migrates the legacy prototype tray on Windows
// (§6.5) and manages the Windows Firewall rule for Syncthing (§5.5).
//
//	Windows: %LOCALAPPDATA%\Programs\SyncThingV2\stv2.exe, the HKCU Uninstall
//	         key, a Start Menu shortcut and the tray's HKCU Run value.
//	macOS:   the tray LaunchAgent; the .app stays where the user put it
//	         (uninstall removes it when it is in ~/Applications).
//	Linux:   ~/.local/share/syncthing-v2/bin/stv2, the ~/.local/bin/stv2
//	         symlink, a .desktop file, hicolor icons and the XDG autostart
//	         entry.
//
// Nothing here ever touches Syncthing's configuration, database or synced
// folders. The managed Syncthing binary (the one SyncThing V2 downloaded) is
// removed only when the user asks for it.
//
// Every location and side effect is a field of Roots, so tests run against
// temporary directories, a test-only registry key and fake processes.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/single"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// Questions asked through Options.Confirm.
const (
	// LegacyQuestion is the one-time legacy-tray migration prompt (§6.5).
	LegacyQuestion = "An older Syncthing tray app is running. Replace it with SyncThing V2?"
	// RemoveSyncthingQuestion is asked by an interactive uninstall when a
	// managed Syncthing exists.
	RemoveSyncthingQuestion = "Also remove the Syncthing program that SyncThing V2 installed? Your Syncthing settings, database and synced folders are kept either way."
)

// quitWait bounds how long Install and Uninstall wait for a running tray to
// exit after /api/quit.
const quitWait = 5 * time.Second

// Process is a running process, as seen by legacy detection.
type Process struct {
	PID  int
	Name string // executable base name, e.g. "SyncthingTray.exe"
}

// Roots are the locations and side effects Install and Uninstall work with.
// Fields for other operating systems are ignored. DefaultRoots returns the
// real locations; tests pass temporary ones.
type Roots struct {
	// InstallDir holds the installed stv2 executable:
	//   Windows: %LOCALAPPDATA%\Programs\SyncThingV2
	//   Linux:   ~/.local/share/syncthing-v2/bin
	//   macOS:   ~/Library/Application Support/SyncThingV2/bin (only used
	//            when stv2 runs outside an .app bundle)
	InstallDir string
	// DataDir is SyncThing V2's per-user data (prefs, logs, instance.json,
	// WebView2 data); osutil.AppDataDir in production.
	DataDir string
	// LogDir is removed on uninstall when it lies outside DataDir (macOS:
	// ~/Library/Logs/SyncThingV2). "" when it is inside DataDir.
	LogDir string
	// ManagedSyncthingDir is where SyncThing V2 installs Syncthing
	// (stinstall.ManagedDir). It is kept on uninstall unless the user asks.
	ManagedSyncthingDir string
	// Autostart manages the tray and Syncthing login entries.
	Autostart *autostart.Manager

	// Windows. UninstallKey is relative to HKEY_CURRENT_USER.
	UninstallKey string
	StartMenuDir string // ...\Start Menu\Programs
	StartupDir   string // ...\Start Menu\Programs\Startup (legacy detection)

	// Linux.
	LinkDir         string   // ~/.local/bin
	ApplicationsDir string   // ~/.local/share/applications
	IconsDir        string   // ~/.local/share/icons/hicolor
	PackagedDirs    []string // prefixes of package-managed binaries (/usr/, /opt/, /snap/)

	// macOS.
	UserApplications string // ~/Applications

	// Side effects. nil fields get production defaults in Install/Uninstall,
	// except Processes, Terminate and FirewallRule, which are skipped when nil.
	Signal        func(ctx context.Context, dataDir, action string) error // single.Signal
	StartDetached func(name string, args ...string) error
	StopSyncthing func(ctx context.Context, bin string) error
	SelfDelete    func(script string) error // Windows: runs `cmd.exe /d /v:off /c <script>` detached and hidden
	Processes     func() ([]Process, error)
	Terminate     func(pid int) error
	FirewallRule  func(ctx context.Context) (bool, error)
}

func (r Roots) withDefaults() Roots {
	if r.Signal == nil {
		r.Signal = single.Signal
	}
	if r.StartDetached == nil {
		r.StartDetached = func(name string, args ...string) error {
			_, err := osutil.StartDetached(name, args...)
			return err
		}
	}
	if r.StopSyncthing == nil {
		r.StopSyncthing = stopSyncthing
	}
	if r.SelfDelete == nil {
		r.SelfDelete = selfDelete
	}
	return r
}

func (r Roots) check() error {
	for name, p := range map[string]string{"InstallDir": r.InstallDir, "DataDir": r.DataDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("install: %s %q is not an absolute path", name, p)
		}
	}
	if r.Autostart == nil {
		return errors.New("install: no autostart manager")
	}
	return nil
}

// Options control Install and Uninstall.
type Options struct {
	// Yes answers the confirmation prompts: install and uninstall proceed
	// without asking; the legacy-tray question is left to the dashboard
	// notice and the managed Syncthing is kept unless RemoveSyncthing.
	Yes bool
	// NoStart skips launching `stv2 --setup` after Install.
	NoStart bool
	// RemoveSyncthing makes Uninstall remove the managed Syncthing binary and
	// its autostart entry too.
	RemoveSyncthing bool
	// Source is the executable to install; "" means the running one.
	Source string
	// Self is the running executable; "" means os.Executable. Uninstall uses
	// it to decide whether the program can be deleted directly.
	Self string
	// Confirm asks the user a yes/no question; nil means "no" to optional
	// steps.
	Confirm func(question string) bool
	// Log receives one line per step; nil discards.
	Log io.Writer
}

func (o Options) withDefaults() (Options, error) {
	if o.Source == "" || o.Self == "" {
		exe, err := os.Executable()
		if err != nil {
			return o, fmt.Errorf("install: cannot find the running executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if o.Source == "" {
			o.Source = exe
		}
		if o.Self == "" {
			o.Self = exe
		}
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	return o, nil
}

func (o Options) logf(format string, args ...any) {
	fmt.Fprintf(o.Log, format+"\n", args...)
}

func (o Options) confirm(q string) bool {
	return !o.Yes && o.Confirm != nil && o.Confirm(q)
}

// Result reports what Install or Uninstall did.
type Result struct {
	Exe            string   // installed executable (Install)
	Upgraded       bool     // an older copy was replaced (Install)
	LegacyMigrated bool     // the legacy tray was stopped and its shortcut disabled
	Started        bool     // `stv2 --setup` was launched (Install)
	SyncthingKept  bool     // the managed Syncthing was left in place (Uninstall)
	Notes          []string // things the user should know
}

// State describes an existing installation, as read by Inspect.
type State struct {
	Installed bool              // SyncThing V2 is installed for this user
	Exe       string            // the executable autostart and shortcuts use, "" when absent
	Version   string            // Windows: DisplayVersion of the Uninstall entry
	Values    map[string]string // Windows: Uninstall key values (DWORDs in decimal)
	Shortcut  string            // Start Menu shortcut (Windows) or .desktop file (Linux), "" when absent
}

// InstalledExe is the path of the installed executable under r.
func InstalledExe(r Roots) string { return filepath.Join(r.InstallDir, brand.ExeName()) }

// IsInstalledCopy reports whether exe is the installed executable.
func IsInstalledCopy(r Roots, exe string) bool { return samePath(exe, InstalledExe(r)) }

// CleanupOld deletes the stv2.exe.old left by an upgrade. The app calls it at
// startup; a missing file is not an error.
func CleanupOld(r Roots) error {
	return removeIfExists(InstalledExe(r) + ".old")
}

// Install installs (or upgrades) SyncThing V2 for the current user:
//
//  1. a running installed tray is asked to quit (/api/quit) and awaited
//  2. the executable is placed (per OS; Windows renames the old copy to .old)
//  3. the per-OS registration is written (Uninstall key and Start Menu
//     shortcut; or symlink, .desktop file and icons)
//  4. the tray autostart entry is set
//  5. the legacy prototype is migrated on consent (Windows)
//  6. unless NoStart, `stv2 --setup` is launched detached
func Install(ctx context.Context, r Roots, o Options) (Result, error) {
	var res Result
	if err := r.check(); err != nil {
		return res, err
	}
	r = r.withDefaults()
	o, err := o.withDefaults()
	if err != nil {
		return res, err
	}
	stopRunning(ctx, r, o)

	exe, upgraded, err := placeBinary(r, o.Source)
	if err != nil {
		return res, err
	}
	res.Exe, res.Upgraded = exe, upgraded
	if upgraded {
		o.logf("Replaced the previous copy at %s", exe)
	} else {
		o.logf("Installed %s", exe)
	}
	notes, err := register(r, exe)
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return res, err
	}
	if err := r.Autostart.Set(autostart.Tray, true, exe, nil); err != nil {
		return res, fmt.Errorf("install: tray autostart: %w", err)
	}
	o.logf("%s will start %s", brand.DisplayName, brand.AtLogin())

	migrated, note, err := legacyStep(r, o)
	res.LegacyMigrated = migrated
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	if err != nil {
		return res, err
	}

	if !o.NoStart {
		if err := r.StartDetached(exe, "--setup"); err != nil {
			return res, fmt.Errorf("install: start %s: %w", brand.DisplayName, err)
		}
		res.Started = true
		o.logf("Started %s", brand.DisplayName)
	}
	return res, nil
}

// legacyStep detects the legacy tray and migrates it when the user agrees.
// The question is asked once (§6.5): after an answer is recorded in prefs
// (LegacyAsked) neither Install nor the dashboard asks again. With
// Options.Yes (or no Confirm) the question is left to the dashboard's
// one-time "legacy-tray" notice.
func legacyStep(r Roots, o Options) (migrated bool, note string, err error) {
	l := DetectLegacy(r)
	if !l.Found() {
		return false, "", nil
	}
	// Unreadable prefs cannot prove the question was answered, so it is asked.
	st, _ := prefs.Open(r.DataDir)
	if st != nil && st.Get().LegacyAsked {
		return false, "", nil
	}
	if o.Yes || o.Confirm == nil {
		return false, "An older Syncthing tray app was found; the dashboard will offer to replace it.", nil
	}
	yes := o.Confirm(LegacyQuestion)
	// Record the answer so it is never asked again.
	if st != nil {
		_ = st.Update(func(p *prefs.Prefs) error { p.LegacyAsked = true; return nil })
	}
	if !yes {
		return false, "", nil
	}
	if err := MigrateLegacy(r, l); err != nil {
		return false, "", err
	}
	o.logf("Stopped the older Syncthing tray app and disabled its startup shortcut")
	return true, "", nil
}

// Uninstall removes what SyncThing V2 installed: the tray autostart entry,
// the per-OS registration, the data directory and the program. The managed
// Syncthing binary and its autostart entry are removed only with
// RemoveSyncthing (or a "yes" to RemoveSyncthingQuestion). Syncthing's
// configuration, database and synced folders are never touched. Every step
// runs even when an earlier one fails; the errors are joined.
func Uninstall(ctx context.Context, r Roots, o Options) (Result, error) {
	var res Result
	if err := r.check(); err != nil {
		return res, err
	}
	r = r.withDefaults()
	o, err := o.withDefaults()
	if err != nil {
		return res, err
	}
	stopRunning(ctx, r, o)

	managed := r.ManagedSyncthingDir != "" && exists(r.ManagedSyncthingDir)
	removeST := o.RemoveSyncthing || (managed && o.confirm(RemoveSyncthingQuestion))

	var errs []error
	if err := r.Autostart.Set(autostart.Tray, false, "", nil); err != nil {
		errs = append(errs, fmt.Errorf("tray autostart: %w", err))
	}
	if removeST {
		if managed {
			bin := filepath.Join(r.ManagedSyncthingDir, syncthingExe())
			if isRegular(bin) {
				if err := r.StopSyncthing(ctx, bin); err != nil {
					o.logf("Could not stop Syncthing: %v", err)
				}
			}
		}
		if err := r.Autostart.Set(autostart.Syncthing, false, "", nil); err != nil {
			errs = append(errs, fmt.Errorf("syncthing autostart: %w", err))
		}
		if managed {
			if err := removeAllRetry(r.ManagedSyncthingDir, 10*time.Second); err != nil {
				errs = append(errs, fmt.Errorf("managed Syncthing: %w", err))
			} else {
				o.logf("Removed the managed Syncthing at %s", r.ManagedSyncthingDir)
			}
		}
	} else if managed {
		res.SyncthingKept = true
		res.Notes = append(res.Notes, "Syncthing (installed by "+brand.DisplayName+") was kept at "+r.ManagedSyncthingDir+" and keeps syncing.")
	}

	if err := unregister(r); err != nil {
		errs = append(errs, err)
	}
	if r.FirewallRule != nil {
		if has, err := r.FirewallRule(ctx); err == nil && has {
			res.Notes = append(res.Notes, `The Windows Firewall rule "`+FirewallRuleName+`" needs administrator rights to remove; remove it in Windows Defender Firewall if you no longer need it.`)
		}
	}

	keep := ""
	if !removeST {
		keep = r.ManagedSyncthingDir
	}
	var leftovers []string
	if err := removeTreeExcept(r.DataDir, keep); err != nil {
		// On Windows, files this process or the exiting tray still hold open
		// cannot be deleted yet; the after-exit script removes the rest. It
		// deletes whole directories, so it is not used when a kept Syncthing
		// lives inside.
		if runtime.GOOS == "windows" && (keep == "" || !within(keep, r.DataDir)) {
			leftovers = append(leftovers, r.DataDir)
			o.logf("Some files in %s are in use; they are removed after exit", r.DataDir)
		} else {
			errs = append(errs, fmt.Errorf("data directory: %w", err))
		}
	}
	if r.LogDir != "" && !within(r.LogDir, r.DataDir) {
		if err := removeLogs(r.LogDir, removeST); err != nil {
			errs = append(errs, err)
		}
	}
	notes, err := removeProgram(r, o, removeST, leftovers)
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		errs = append(errs, err)
	}
	o.logf("%s was uninstalled", brand.DisplayName)
	return res, errors.Join(errs...)
}

// stopRunning asks a running tray (instance.json in DataDir) to quit and
// waits up to quitWait for it to exit. Our own process is never signalled.
func stopRunning(ctx context.Context, r Roots, o Options) {
	in, err := single.ReadInstance(r.DataDir)
	if err != nil || in.PID == os.Getpid() {
		return
	}
	if err := r.Signal(ctx, r.DataDir, "quit"); err != nil {
		o.logf("The running %s did not answer: %v", brand.DisplayName, err)
	}
	if !waitExit(ctx, in.PID, quitWait) {
		o.logf("The running %s (PID %d) did not exit within %s", brand.DisplayName, in.PID, quitWait)
	}
}

func waitExit(ctx context.Context, pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for processAlive(pid) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// stopSyncthing is the production Roots.StopSyncthing: it asks the
// Syncthing that uses bin's config to shut down through its REST API and
// waits for the API to stop answering. A Syncthing that is not running is
// not an error.
func stopSyncthing(ctx context.Context, bin string) error {
	ep, _, err := stclient.Discover(ctx, bin)
	if errors.Is(err, stclient.ErrConfigNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c := stclient.New(ep)
	if err := c.Send(ctx, http.MethodPost, "/rest/system/shutdown", nil); err != nil {
		if k, ok := stclient.KindOf(err); ok && k == stclient.ErrUnreachable {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		var v struct{}
		if err := c.Get(ctx, "/rest/system/ping", &v); err != nil {
			if k, ok := stclient.KindOf(err); ok && k == stclient.ErrUnreachable {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("syncthing did not stop within 10 s")
}

// copyExecutable copies src to dst through a temporary file in dst's
// directory and a rename, with mode 0755.
func copyExecutable(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		return err
	}
	if err = tmp.Chmod(0o755); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// removeTreeExcept removes dir except keep (and the directories leading to
// it) when keep lies inside dir and exists. dir itself goes when it ends up
// empty. A missing dir is not an error.
func removeTreeExcept(dir, keep string) error {
	if !exists(dir) {
		return nil
	}
	if keep == "" || !within(keep, dir) || samePath(keep, dir) || !exists(keep) {
		return os.RemoveAll(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case samePath(p, keep):
			continue
		case within(keep, p) && e.IsDir():
			errs = append(errs, removeTreeExcept(p, keep))
		default:
			errs = append(errs, os.RemoveAll(p))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return removeIfEmpty(dir)
}

// removeLogs removes a log directory outside DataDir. When Syncthing is kept
// its own log (syncthing.log*, written by the LaunchAgent) stays.
func removeLogs(dir string, all bool) error {
	if all {
		return os.RemoveAll(dir)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e.Name()), "syncthing") {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return removeIfEmpty(dir)
}

// removeAllRetry is os.RemoveAll retried for up to d, because a program that
// just exited can keep its files locked for a moment on Windows.
func removeAllRetry(dir string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		err := os.RemoveAll(dir)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func removeIfEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || len(entries) > 0 {
		return err
	}
	return os.Remove(dir)
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isRegular(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func syncthingExe() string {
	if runtime.GOOS == "windows" {
		return "syncthing.exe"
	}
	return "syncthing"
}

// samePath compares cleaned paths, ignoring case on Windows and macOS.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether p is dir or lies below it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		if r2, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(p)); err == nil {
			rel = r2
		}
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// ---------------------------------------------------------------------------
// Legacy prototype (§6.5). The Windows files supply the real process list.

// Legacy prototype names.
const (
	LegacyShortcut = "Syncthing Tray.lnk"
	LegacyProcess  = "SyncthingTray.exe"
)

// Legacy is what legacy detection found.
type Legacy struct {
	Shortcut string // path of Startup\Syncthing Tray.lnk, "" if absent
	PIDs     []int  // running SyncthingTray.exe processes
}

// Found reports whether the legacy tray is installed or running.
func (l Legacy) Found() bool { return l.Shortcut != "" || len(l.PIDs) > 0 }

func detectLegacy(startupDir string, procs func() ([]Process, error)) Legacy {
	var l Legacy
	if startupDir != "" {
		p := filepath.Join(startupDir, LegacyShortcut)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			l.Shortcut = p
		}
	}
	if procs != nil {
		if list, err := procs(); err == nil {
			for _, pr := range list {
				if strings.EqualFold(pr.Name, LegacyProcess) && pr.PID != os.Getpid() {
					l.PIDs = append(l.PIDs, pr.PID)
				}
			}
		}
	}
	slices.Sort(l.PIDs)
	return l
}

// migrateLegacy terminates the legacy processes and renames the shortcut to
// "Syncthing Tray.lnk.disabled", which is reversible by renaming it back.
func migrateLegacy(l Legacy, terminate func(int) error) error {
	var errs []error
	for _, pid := range l.PIDs {
		if terminate == nil {
			errs = append(errs, fmt.Errorf("cannot stop the legacy tray (PID %d)", pid))
			continue
		}
		if err := terminate(pid); err != nil {
			errs = append(errs, fmt.Errorf("stop the legacy tray (PID %d): %w", pid, err))
		}
	}
	if l.Shortcut != "" {
		if err := os.Rename(l.Shortcut, l.Shortcut+".disabled"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("disable the legacy startup shortcut: %w", err))
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Windows Firewall rule (§5.5). The command lines are built here so they can
// be tested on every OS; the Windows files run them.

// FirewallRuleName is the name of the only firewall rule SyncThing V2 owns.
const FirewallRuleName = brand.DisplayName + " - Syncthing"

// ErrElevationDeclined means the user cancelled the UAC prompt.
var ErrElevationDeclined = errors.New("administrator permission was not granted")

// ErrFirewallUnsupported is returned by the firewall functions off Windows.
var ErrFirewallUnsupported = errors.New("firewall allow is only available on Windows; see docs/troubleshooting.md for macOS and Linux")

// ErrNotElevated is returned by the elevated child when it still lacks
// administrator rights (for example with UAC turned off for a standard
// user), instead of re-launching itself again.
var ErrNotElevated = errors.New("administrator rights were not obtained")

// ElevatedFlag marks the re-launched, elevated `stv2 firewall allow`.
const ElevatedFlag = "--elevated"

// ElevatedFirewallArgs is the command line (after the executable) that the
// non-elevated `stv2 firewall allow` re-launches with the "runas" verb. The
// CLI must route it (recognised by ElevatedFlag) to
// AllowFirewallElevated(ctx, bin), which never re-launches.
func ElevatedFirewallArgs(bin string) []string {
	return []string{"firewall", "allow", "--bin", bin, ElevatedFlag}
}

// firewallAddArgs is the netsh argument string for one protocol, exactly as
// in spec §5.5.
func firewallAddArgs(bin, proto string) (string, error) {
	if err := checkCmdPath(bin); err != nil {
		return "", err
	}
	if proto != "TCP" && proto != "UDP" {
		return "", fmt.Errorf("firewall: unknown protocol %q", proto)
	}
	return `advfirewall firewall add rule name="` + FirewallRuleName + `" dir=in action=allow program="` + bin +
		`" protocol=` + proto + ` localport=22000 remoteip=100.64.0.0/10,LocalSubnet enable=yes`, nil
}

func firewallDeleteArgs() string {
	return `advfirewall firewall delete rule name="` + FirewallRuleName + `"`
}

func firewallShowArgs() string {
	return `advfirewall firewall show rule name="` + FirewallRuleName + `"`
}

// firewall runs the rule logic through injectable seams.
type firewall struct {
	elevated func() bool
	// netsh runs netsh.exe with the given argument string and returns its
	// combined output and exit code; err is set only when it could not run.
	netsh    func(ctx context.Context, args string) (out []byte, code int, err error)
	relaunch func(ctx context.Context, args []string) error
	// child is set in the re-launched process: without elevation it fails
	// with ErrNotElevated rather than re-launching again.
	child bool
}

// allow adds the TCP and UDP rules for bin. Without elevation it re-launches
// the executable elevated with ElevatedFirewallArgs (unless it is that
// re-launched child). An existing rule of ours is replaced, so running it
// twice leaves one rule per protocol.
func (f firewall) allow(ctx context.Context, bin string) error {
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("firewall: %q is not an absolute path", bin)
	}
	bin = filepath.Clean(bin)
	var lines []string
	for _, proto := range []string{"TCP", "UDP"} {
		l, err := firewallAddArgs(bin, proto)
		if err != nil {
			return err
		}
		lines = append(lines, l)
	}
	if !f.elevated() {
		if f.child {
			return ErrNotElevated
		}
		return f.relaunch(ctx, ElevatedFirewallArgs(bin))
	}
	// Exit code 1 means "no rule matched", which is fine.
	if _, _, err := f.netsh(ctx, firewallDeleteArgs()); err != nil {
		return err
	}
	for _, l := range lines {
		out, code, err := f.netsh(ctx, l)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("netsh failed (exit code %d): %s", code, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// hasRule reports whether our rule exists; it checks our rule name only.
func (f firewall) hasRule(ctx context.Context) (bool, error) {
	_, code, err := f.netsh(ctx, firewallShowArgs())
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("netsh show rule failed (exit code %d)", code)
}

// selfDeleteCommandLine is the verbatim cmd.exe command line for the
// self-delete script. /d skips the AutoRun commands and /v:off turns delayed
// expansion off even when the Command Processor DelayedExpansion policy turns
// it on, so a '!' in a quoted path stays literal ('%' and '"' are refused by
// checkCmdPath).
func selfDeleteCommandLine(comspec, script string) string {
	return `"` + comspec + `" /d /v:off /c ` + script
}

// checkCmdPath rejects paths that cannot be embedded safely in a quoted
// netsh or cmd.exe argument.
func checkCmdPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%q is not an absolute path", p)
	}
	if strings.ContainsAny(p, "\"%\r\n\x00") {
		return fmt.Errorf("path %q contains characters that cannot be quoted safely", p)
	}
	return nil
}
