// Package stinstall finds, installs and starts Syncthing (spec §3.4, §5.1,
// §5.2). An adopted Syncthing (one SyncThing V2 did not install) is only read,
// never modified. When Syncthing is missing, SyncThing V2 downloads the pinned
// upstream release, verifies its SHA-256 against pins.go, extracts it into a
// user-writable "managed" directory, generates a config and starts it.
package stinstall

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// Install describes the Syncthing that SyncThing V2 works with.
type Install struct {
	Bin        string // absolute path of the syncthing executable
	ConfigPath string // config.xml in use, "" when none was found
	Managed    bool   // installed by SyncThing V2 into ManagedDir
	Version    string // for example "v2.1.5"; "" when `--version` failed
	Running    bool   // a syncthing process was running when Detect ran
	// AutostartExists is true when Syncthing already starts at login, through
	// SyncThing V2's own entry or one configured elsewhere.
	AutostartExists bool
}

// ErrNotFound is returned by Detect when no Syncthing executable exists.
var ErrNotFound = errors.New("syncthing not found")

// versionTimeout bounds `syncthing --version`.
const versionTimeout = 10 * time.Second

// Detect finds Syncthing, taking the first match of (§5.1):
//
//  1. the executable of a running syncthing process
//  2. syncthing on PATH
//  3. the per-OS known install locations
//  4. the managed location (Managed = true)
//
// A binary found by steps 1–3 that is the managed binary is still reported
// as Managed. When nothing is found the error wraps ErrNotFound.
func Detect(ctx context.Context) (Install, error) {
	return defaultDetector().detect(ctx)
}

// ExeName is the Syncthing executable name on the running OS.
func ExeName() string { return exeName(runtime.GOOS) }

func exeName(goos string) string {
	if goos == "windows" {
		return "syncthing.exe"
	}
	return "syncthing"
}

// ManagedDir returns the directory SyncThing V2 installs Syncthing into:
//
//	Windows: %LOCALAPPDATA%\Programs\SyncThingV2\syncthing
//	macOS:   ~/Library/Application Support/SyncThingV2/syncthing
//	Linux:   ~/.local/share/syncthing-v2/syncthing ($XDG_DATA_HOME respected)
func ManagedDir() (string, error) {
	return managedDir(runtime.GOOS, os.Getenv, osutil.HomeDir)
}

// ManagedBin returns the path of the managed Syncthing executable.
func ManagedBin() (string, error) {
	d, err := ManagedDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, ExeName()), nil
}

func managedDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	switch goos {
	case "windows":
		lad, err := localAppData(getenv, home)
		if err != nil {
			return "", err
		}
		return filepath.Join(lad, "Programs", brand.AppDirName(), "syncthing"), nil
	case "darwin":
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, "Library", "Application Support", brand.AppDirName(), "syncthing"), nil
	default:
		base := getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(base) {
			h, err := home()
			if err != nil {
				return "", err
			}
			base = filepath.Join(h, ".local", "share")
		}
		return filepath.Join(base, brand.AppDirName(), "syncthing"), nil
	}
}

func localAppData(getenv func(string) string, home func() (string, error)) (string, error) {
	if p := getenv("LOCALAPPDATA"); filepath.IsAbs(p) {
		return p, nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "AppData", "Local"), nil
}

// knownPaths lists the per-OS install locations of step 3 of Detect.
func knownPaths(goos string, getenv func(string) string, home func() (string, error)) []string {
	h, _ := home()
	join := func(base string, elem ...string) string {
		if base == "" {
			return ""
		}
		return filepath.Join(append([]string{base}, elem...)...)
	}
	switch goos {
	case "windows":
		lad, _ := localAppData(getenv, home)
		profile := getenv("USERPROFILE")
		if !filepath.IsAbs(profile) {
			profile = h
		}
		return []string{
			join(lad, "Programs", "Syncthing", "syncthing.exe"),
			join(profile, "scoop", "shims", "syncthing.exe"),
			join(lad, "Microsoft", "WinGet", "Links", "syncthing.exe"),
		}
	case "darwin":
		return []string{
			"/opt/homebrew/bin/syncthing",
			"/usr/local/bin/syncthing",
			"/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing",
			"/opt/local/bin/syncthing", // MacPorts
			join(h, ".nix-profile", "bin", "syncthing"),
		}
	default:
		return []string{
			"/usr/bin/syncthing",
			join(h, ".local", "bin", "syncthing"),
			"/snap/bin/syncthing",
			"/usr/local/bin/syncthing",
			"/home/linuxbrew/.linuxbrew/bin/syncthing",
			join(h, ".linuxbrew", "bin", "syncthing"),
			join(h, ".nix-profile", "bin", "syncthing"),
		}
	}
}

// detector holds everything Detect depends on, so tests can replace it.
type detector struct {
	goos      string
	getenv    func(string) string
	home      func() (string, error)
	running   func(ctx context.Context) []string // executables of running syncthing processes
	lookPath  func(file string) (string, error)
	isFile    func(path string) bool
	version   func(ctx context.Context, bin string) string
	config    func(ctx context.Context, bin string) string
	autostart func() bool
	resolve   func(path string) (string, error) // filepath.EvalSymlinks
}

func defaultDetector() detector {
	return detector{
		goos:      runtime.GOOS,
		getenv:    os.Getenv,
		home:      osutil.HomeDir,
		running:   runningSyncthing,
		lookPath:  exec.LookPath,
		isFile:    trustedBin,
		version:   BinVersion,
		config:    configPath,
		autostart: syncthingAutostartExists,
		resolve:   filepath.EvalSymlinks,
	}
}

func (d detector) detect(ctx context.Context) (Install, error) {
	managed, merr := managedDir(d.goos, d.getenv, d.home)
	managedBin := ""
	if merr == nil {
		managedBin = filepath.Join(managed, exeName(d.goos))
	}

	var in Install
	for _, p := range d.running(ctx) {
		if p = d.usable(p); p != "" {
			in.Bin, in.Running = d.stableLink(p), true
			break
		}
	}
	if in.Bin == "" {
		if p, err := d.lookPath(exeName(d.goos)); err == nil {
			in.Bin = d.usable(p)
		}
	}
	if in.Bin == "" {
		for _, p := range knownPaths(d.goos, d.getenv, d.home) {
			if p = d.usable(p); p != "" {
				in.Bin = p
				break
			}
		}
	}
	if in.Bin == "" && managedBin != "" {
		in.Bin = d.usable(managedBin)
	}
	if in.Bin == "" {
		if err := ctx.Err(); err != nil {
			return Install{}, err
		}
		return Install{}, ErrNotFound
	}

	in.Managed = managedBin != "" && samePath(d.goos, in.Bin, managedBin)
	in.Version = d.version(ctx, in.Bin)
	in.ConfigPath = d.config(ctx, in.Bin)
	// A Syncthing inside another app's bundle (Syncthing.app) is started at
	// login by that app's own login item.
	in.AutostartExists = d.autostart() || (d.goos == "darwin" && autostart.AppBundle(in.Bin) != "")
	return in, nil
}

// versioned reports whether p lies in a package manager's versioned store,
// which the next upgrade or garbage collection deletes: Homebrew's Cellar
// (Linuxbrew and macOS) and the Nix store.
func versioned(p string) bool {
	s := filepath.ToSlash(p)
	return strings.Contains(s, "/nix/store/") || strings.Contains(s, "/Cellar/")
}

// stableLink maps a running Syncthing's resolved executable (Linux reads it
// from /proc/<pid>/exe, which follows symlinks) back to the stable symlink
// that points at it, such as /home/linuxbrew/.linuxbrew/bin/syncthing or
// ~/.nix-profile/bin/syncthing, so that login entries survive the next
// upgrade. It returns bin unchanged when bin is not in a versioned store or
// no candidate on PATH or in the known locations resolves to it.
func (d detector) stableLink(bin string) string {
	if !versioned(bin) || d.resolve == nil {
		return bin
	}
	var cands []string
	if p, err := d.lookPath(exeName(d.goos)); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, knownPaths(d.goos, d.getenv, d.home)...)
	for _, c := range cands {
		c = d.usable(c)
		if c == "" || versioned(c) {
			continue
		}
		if real, err := d.resolve(c); err == nil && samePath(d.goos, real, bin) {
			return c
		}
	}
	return bin
}

// usable returns the cleaned absolute path when p is an existing regular
// file that is safe to run as this user (see trustedBin), and "" otherwise.
func (d detector) usable(p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return ""
		}
		p = abs
	}
	p = filepath.Clean(p)
	if !d.isFile(p) {
		return ""
	}
	return p
}

func samePath(goos, a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if goos == "windows" || goos == "darwin" { // case-insensitive file systems by default
		return strings.EqualFold(a, b)
	}
	return a == b
}

func isRegular(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// BinVersion runs `<bin> --version` and returns the version it prints (for
// example "v2.1.5"), or "" when the command fails or prints none.
func BinVersion(ctx context.Context, bin string) string {
	out, err := osutil.Output(ctx, versionTimeout, bin, "--version")
	if err != nil && len(out) == 0 {
		return ""
	}
	return ParseVersionOutput(string(out))
}

// configPath returns the config.xml Syncthing uses, or "" when there is
// none. A config that exists but cannot be parsed is still returned; the
// caller reports the parse error when it connects.
func configPath(ctx context.Context, bin string) string {
	_, p, _ := stclient.Discover(ctx, bin)
	return p
}

func syncthingAutostartExists() bool {
	if on, err := autostart.Enabled(autostart.Syncthing); err == nil && on {
		return true
	}
	_, ok := autostart.Existing(autostart.Syncthing)
	return ok
}

// procSyncthing scans a /proc tree for processes whose executable is named
// syncthing. Processes of other users whose exe link cannot be read are
// skipped. A Syncthing running from inside a snap is reported as its
// /snap/bin launcher (see snapLauncher), and skipped when isFile reports that
// the launcher does not exist.
func procSyncthing(ctx context.Context, proc string, isFile func(string) bool) []string {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if ctx.Err() != nil {
			return out
		}
		if !isPID(e.Name()) {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "syncthing" {
			continue
		}
		exe, err := os.Readlink(filepath.Join(proc, e.Name(), "exe"))
		if err != nil {
			continue
		}
		// After a self-upgrade the old image is unlinked; its path still
		// names the binary that now sits there.
		exe = strings.TrimSuffix(exe, " (deleted)")
		if path.Base(exe) != "syncthing" {
			continue
		}
		if launcher, isSnap := snapLauncher(exe); isSnap {
			if !isFile(launcher) {
				continue
			}
			exe = launcher
		}
		out = append(out, exe)
	}
	return out
}

// snapLauncher maps an executable inside a snap (/snap/<name>/<revision>/...)
// to the snap's launcher /snap/bin/<name>. Run directly, the inner binary
// lacks the snap's confinement and environment, so it looks for its config in
// the wrong place, and its revision directory disappears on the next snap
// refresh. isSnap is false for paths outside /snap.
func snapLauncher(exe string) (launcher string, isSnap bool) {
	exe = path.Clean(exe)
	rest, ok := strings.CutPrefix(exe, "/snap/")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "bin" { // already a launcher
		return exe, true
	}
	return "/snap/bin/" + name, true
}

// parsePS parses `ps -axo pid=,uid=,comm=` output and returns the absolute
// paths whose base name is syncthing, of processes run by uid. A syncthing
// that another user runs is never adopted: its binary is theirs to replace.
func parsePS(out []byte, uid int) []string {
	want := strconv.Itoa(uid)
	var paths []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		pid, rest, ok := strings.Cut(line, " ")
		if !ok || !isPID(pid) {
			continue
		}
		owner, comm, ok := strings.Cut(strings.TrimSpace(rest), " ")
		if !ok || owner != want {
			continue
		}
		comm = strings.TrimSpace(comm)
		if path.IsAbs(comm) && path.Base(comm) == "syncthing" { // POSIX paths
			paths = append(paths, comm)
		}
	}
	return paths
}

func isPID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
