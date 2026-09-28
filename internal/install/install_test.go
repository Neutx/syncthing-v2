package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/single"
)

// recorder captures the side effects Install and Uninstall request.
type recorder struct {
	mu       sync.Mutex
	started  [][]string
	signals  []string
	stopped  []string
	scripts  []string
	killed   []int
	procs    []Process
	fwChecks int
}

func (rc *recorder) roots(r Roots) Roots {
	r.StartDetached = func(name string, args ...string) error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.started = append(rc.started, append([]string{name}, args...))
		return nil
	}
	r.Signal = func(_ context.Context, dir, action string) error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.signals = append(rc.signals, action)
		return nil
	}
	r.StopSyncthing = func(_ context.Context, bin string) error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.stopped = append(rc.stopped, bin)
		return nil
	}
	r.SelfDelete = func(script string) error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.scripts = append(rc.scripts, script)
		return nil
	}
	r.Processes = func() ([]Process, error) { return rc.procs, nil }
	r.Terminate = func(pid int) error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.killed = append(rc.killed, pid)
		return nil
	}
	r.FirewallRule = func(context.Context) (bool, error) {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.fwChecks++
		return false, nil
	}
	return r
}

// testRoots lays out a per-OS install under root. On Windows the registry
// keys live below a random HKCU\Software\SyncThingV2-test-<hex> key, deleted
// when the test ends.
func testRoots(t *testing.T, root string) Roots {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		base := `Software\SyncThingV2-test-` + randHex(t)
		t.Cleanup(func() {
			// reg.exe deletes the whole test tree; nothing outside it is touched.
			_ = exec.Command("reg.exe", "delete", `HKCU\`+base, "/f").Run()
		})
		install := filepath.Join(root, "Local", "Programs", "SyncThingV2")
		startup := filepath.Join(root, "StartMenu", "Programs", "Startup")
		return Roots{
			InstallDir:          install,
			DataDir:             filepath.Join(root, "Local", "SyncThingV2"),
			ManagedSyncthingDir: filepath.Join(install, "syncthing"),
			Autostart:           autostart.New(autostart.Roots{RunKey: base + `\Run`, StartupDir: startup}),
			UninstallKey:        base + `\Uninstall\SyncThingV2`,
			StartMenuDir:        filepath.Join(root, "StartMenu", "Programs"),
			StartupDir:          startup,
			LegacyExe:           filepath.Join(root, "Local", "Programs", "Syncthing", "tray", "SyncthingTray.exe"),
		}
	case "darwin":
		data := filepath.Join(root, "Library", "Application Support", "SyncThingV2")
		logs := filepath.Join(root, "Library", "Logs", "SyncThingV2")
		return Roots{
			InstallDir:          filepath.Join(data, "bin"),
			DataDir:             data,
			LogDir:              logs,
			ManagedSyncthingDir: filepath.Join(data, "syncthing"),
			Autostart:           autostart.New(autostart.Roots{LaunchAgents: filepath.Join(root, "Library", "LaunchAgents"), LogDir: logs}),
			UserApplications:    filepath.Join(root, "Applications"),
		}
	default:
		data := filepath.Join(root, "share", "syncthing-v2")
		return Roots{
			InstallDir:          filepath.Join(data, "bin"),
			DataDir:             data,
			ManagedSyncthingDir: filepath.Join(data, "syncthing"),
			Autostart:           autostart.New(autostart.Roots{ConfigHome: filepath.Join(root, "config")}),
			LinkDir:             filepath.Join(root, "bin"),
			ApplicationsDir:     filepath.Join(root, "share", "applications"),
			IconsDir:            filepath.Join(root, "share", "icons", "hicolor"),
			PackagedDirs:        []string{filepath.Join(root, "usr") + "/"},
		}
	}
}

func randHex(t *testing.T) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// treeHash hashes every path and file content under dir.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h.Write([]byte(rel + "\x00"))
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// listTree returns the relative paths below dir, or nil when dir is gone.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func enabled(t *testing.T, r Roots, tg autostart.Target) bool {
	t.Helper()
	on, err := r.Autostart.Enabled(tg)
	if err != nil {
		t.Fatal(err)
	}
	return on
}

// fakeSource writes a fake stv2 executable where the OS would run it from.
func fakeSource(t *testing.T, root, content string) string {
	var p string
	switch runtime.GOOS {
	case "windows":
		p = filepath.Join(root, "Downloads", "SyncThingV2-Setup-1.0.0-windows-x64.exe")
	default:
		p = filepath.Join(root, "Downloads", "stv2")
	}
	writeFile(t, p, content)
	return p
}

func TestInstallUninstallRoundTrip(t *testing.T) {
	root := t.TempDir()
	rc := &recorder{}
	r := rc.roots(testRoots(t, root))
	ctx := context.Background()
	self := filepath.Join(root, "elsewhere", brand.ExeName()) // the running binary is not the installed one

	// A Syncthing config and synced data that must never change.
	stHome := filepath.Join(root, "SyncthingHome")
	writeFile(t, filepath.Join(stHome, "config.xml"), "<configuration><gui><apikey>synthetic</apikey></gui></configuration>")
	writeFile(t, filepath.Join(stHome, "index-v2", "db"), "synthetic database")
	writeFile(t, filepath.Join(root, "Sync", "notes.txt"), "synced file")
	stBefore, syncBefore := treeHash(t, stHome), treeHash(t, filepath.Join(root, "Sync"))

	// Fresh install.
	src := fakeSource(t, root, "stv2 build 1")
	res, err := Install(ctx, r, Options{Yes: true, Source: src, Self: self})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	exe := InstalledExe(r)
	if res.Exe != exe || res.Upgraded || !res.Started {
		t.Fatalf("Install result = %+v", res)
	}
	if got := readFile(t, exe); got != "stv2 build 1" {
		t.Fatalf("installed content = %q", got)
	}
	if len(rc.started) != 1 || !slices.Equal(rc.started[0], []string{exe, "--setup"}) {
		t.Fatalf("started = %q, want [%s --setup]", rc.started, exe)
	}
	if !enabled(t, r, autostart.Tray) {
		t.Fatal("tray autostart not set")
	}
	if !IsInstalledCopy(r, exe) || IsInstalledCopy(r, src) {
		t.Error("IsInstalledCopy is wrong")
	}
	checkRegistration(t, r, exe)

	// Upgrade while "running": instance.json points at a PID that has exited.
	if err := single.WriteInstance(r.DataDir, single.Instance{Port: 18999, PID: 0x7ffffff0, Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	src2 := fakeSource(t, root, "stv2 build 2")
	res, err = Install(ctx, r, Options{Yes: true, NoStart: true, Source: src2, Self: self})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !res.Upgraded || res.Started || len(rc.started) != 1 {
		t.Fatalf("upgrade result = %+v, started = %q", res, rc.started)
	}
	if !slices.Equal(rc.signals, []string{"quit"}) {
		t.Fatalf("signals = %q, want the running tray asked to quit", rc.signals)
	}
	if got := readFile(t, exe); got != "stv2 build 2" {
		t.Fatalf("upgraded content = %q", got)
	}
	if runtime.GOOS == "windows" {
		if got := readFile(t, exe+".old"); got != "stv2 build 1" {
			t.Fatalf(".old content = %q", got)
		}
		if err := CleanupOld(r); err != nil || exists(exe+".old") {
			t.Fatalf("CleanupOld: %v", err)
		}
	}
	if err := CleanupOld(r); err != nil {
		t.Fatalf("CleanupOld without a file: %v", err)
	}
	checkRegistration(t, r, exe)

	// A managed Syncthing with its autostart entry, some app data and logs.
	stBin := filepath.Join(r.ManagedSyncthingDir, syncthingExe())
	writeFile(t, stBin, "managed syncthing")
	if err := r.Autostart.Set(autostart.Syncthing, true, stBin, nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.DataDir, "prefs.json"), "{}")
	writeFile(t, filepath.Join(r.DataDir, "logs", "stv2.log"), "log line")
	writeFile(t, filepath.Join(r.DataDir, "WebView2", "state"), "webview data")
	if r.LogDir != "" {
		writeFile(t, filepath.Join(r.LogDir, "stv2.log"), "tray log")
		writeFile(t, filepath.Join(r.LogDir, "syncthing.log"), "syncthing log")
	}

	// Uninstall, keeping Syncthing (the default with --yes).
	res, err = Uninstall(ctx, r, Options{Yes: true, Self: self})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.SyncthingKept || len(rc.stopped) != 0 || len(rc.scripts) != 0 {
		t.Fatalf("uninstall result = %+v, stopped = %q, scripts = %q", res, rc.stopped, rc.scripts)
	}
	if enabled(t, r, autostart.Tray) {
		t.Error("tray autostart left behind")
	}
	if !enabled(t, r, autostart.Syncthing) {
		t.Error("the kept Syncthing lost its autostart entry")
	}
	if got := readFile(t, stBin); got != "managed syncthing" {
		t.Errorf("managed Syncthing changed: %q", got)
	}
	checkUnregistered(t, r)
	wantKept := []string{"syncthing", "syncthing/" + syncthingExe()}
	switch runtime.GOOS {
	case "windows":
		if got := listTree(t, r.InstallDir); !slices.Equal(got, wantKept) {
			t.Errorf("install dir after uninstall = %q, want only the managed Syncthing", got)
		}
		if got := listTree(t, r.DataDir); got != nil {
			t.Errorf("data dir left behind: %q", got)
		}
	default:
		if got := listTree(t, r.DataDir); !slices.Equal(got, wantKept) {
			t.Errorf("data dir after uninstall = %q, want only the managed Syncthing", got)
		}
	}
	if r.LogDir != "" {
		if got := listTree(t, r.LogDir); !slices.Equal(got, []string{"syncthing.log"}) {
			t.Errorf("log dir after uninstall = %q, want only Syncthing's log", got)
		}
	}

	// Reinstall, then uninstall with --remove-syncthing: nothing is left.
	if _, err := Install(ctx, r, Options{Yes: true, NoStart: true, Source: src2, Self: self}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	res, err = Uninstall(ctx, r, Options{Yes: true, RemoveSyncthing: true, Self: self})
	if err != nil {
		t.Fatalf("Uninstall --remove-syncthing: %v", err)
	}
	if res.SyncthingKept || !slices.Equal(rc.stopped, []string{stBin}) {
		t.Fatalf("result = %+v, stopped = %q", res, rc.stopped)
	}
	if enabled(t, r, autostart.Tray) || enabled(t, r, autostart.Syncthing) {
		t.Error("autostart entries left behind")
	}
	checkUnregistered(t, r)
	for _, d := range []string{r.InstallDir, r.DataDir, r.LogDir} {
		if d != "" && exists(d) {
			t.Errorf("%s left behind: %q", d, listTree(t, d))
		}
	}
	if runtime.GOOS == "windows" && rc.fwChecks != 2 {
		t.Errorf("firewall rule checked %d times, want once per uninstall", rc.fwChecks)
	}

	// Syncthing's config and the synced data are untouched.
	if treeHash(t, stHome) != stBefore || treeHash(t, filepath.Join(root, "Sync")) != syncBefore {
		t.Fatal("Syncthing config or synced data changed")
	}
	if len(rc.scripts) != 0 {
		t.Errorf("self-delete scheduled although the running binary is elsewhere: %q", rc.scripts)
	}
}

// checkRegistration verifies the per-OS registration of exe.
func checkRegistration(t *testing.T, r Roots, exe string) {
	t.Helper()
	st := Inspect(r)
	if !st.Installed {
		t.Fatalf("Inspect: not installed: %+v", st)
	}
	switch runtime.GOOS {
	case "windows":
		want := map[string]string{
			"DisplayName":          brand.DisplayName,
			"DisplayVersion":       strings.TrimPrefix(brand.Version, "v"),
			"Publisher":            "SyncThing V2 contributors",
			"DisplayIcon":          exe + ",0",
			"InstallLocation":      r.InstallDir,
			"UninstallString":      `"` + exe + `" uninstall`,
			"QuietUninstallString": `"` + exe + `" uninstall --yes`,
			"NoModify":             "1",
			"NoRepair":             "1",
			"EstimatedSize":        "1",
		}
		for k, v := range want {
			if st.Values[k] != v {
				t.Errorf("Uninstall value %s = %q, want %q", k, st.Values[k], v)
			}
		}
		if len(st.Values) != len(want) {
			t.Errorf("Uninstall values = %v, want exactly %d", st.Values, len(want))
		}
		lnk := filepath.Join(r.StartMenuDir, "SyncThing V2.lnk")
		if st.Shortcut != lnk {
			t.Fatalf("Shortcut = %q, want %q", st.Shortcut, lnk)
		}
		data, err := os.ReadFile(lnk)
		if err != nil {
			t.Fatal(err)
		}
		link, err := autostart.ParseShellLink(data)
		if err != nil {
			t.Fatal(err)
		}
		// IShellLinkW stores the long form of an 8.3 path (a CI runner's
		// TEMP is C:\Users\RUNNER~1\...), so compare files, not spellings.
		if !strings.EqualFold(link.Target, exe) && !sameFile(link.Target, exe) {
			t.Errorf("shortcut target = %q, want %q", link.Target, exe)
		}
	case "linux":
		link := filepath.Join(r.LinkDir, "stv2")
		if target, err := os.Readlink(link); err != nil || target != exe {
			t.Errorf("symlink %s -> %q (%v), want %q", link, target, err, exe)
		}
		desktop := readFile(t, filepath.Join(r.ApplicationsDir, "stv2.desktop"))
		if !strings.Contains(desktop, "\nExec="+exe+"\n") || !strings.Contains(desktop, "\nIcon=stv2\n") {
			t.Errorf("desktop file:\n%s", desktop)
		}
		for _, n := range []string{"16", "48", "512"} {
			f, err := os.Open(filepath.Join(r.IconsDir, n+"x"+n, "apps", "stv2.png"))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := png.DecodeConfig(f)
			f.Close()
			if err != nil || n != strconv.Itoa(cfg.Width) || cfg.Height != cfg.Width {
				t.Errorf("icon %s: %+v, %v", n, cfg, err)
			}
		}
	}
}

func checkUnregistered(t *testing.T, r Roots) {
	t.Helper()
	st := Inspect(r)
	if st.Installed || st.Values != nil || st.Shortcut != "" {
		t.Errorf("registration left behind: %+v", st)
	}
	if runtime.GOOS == "linux" {
		if exists(filepath.Join(r.LinkDir, "stv2")) {
			t.Error("symlink left behind")
		}
		if got := listTree(t, r.IconsDir); slices.ContainsFunc(got, func(p string) bool { return strings.HasSuffix(p, ".png") }) {
			t.Errorf("icons left behind: %q", got)
		}
	}
}

func TestUninstallSelfDelete(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("self-delete through cmd.exe is Windows only; Unix unlinks the running binary")
	}
	root := t.TempDir()
	rc := &recorder{}
	r := rc.roots(testRoots(t, root))
	ctx := context.Background()
	src := fakeSource(t, root, "stv2")
	if _, err := Install(ctx, r, Options{Yes: true, NoStart: true, Source: src, Self: src}); err != nil {
		t.Fatal(err)
	}
	exe := InstalledExe(r)

	// Running from the installed copy, removing everything.
	res, err := Uninstall(ctx, r, Options{Yes: true, RemoveSyncthing: true, Self: exe})
	if err != nil {
		t.Fatal(err)
	}
	// ping runs by its full path in the system directory, never by a bare
	// name cmd.exe would first look up in its current directory.
	if len(rc.scripts) != 1 {
		t.Fatalf("scripts = %q, want one", rc.scripts)
	}
	ping, _, _ := strings.Cut(rc.scripts[0], " -n 3 ")
	sysPing := strings.Trim(ping, `"`)
	if ping != `"`+sysPing+`"` || !filepath.IsAbs(sysPing) || !strings.EqualFold(filepath.Base(sysPing), "PING.EXE") || !isRegular(sysPing) {
		t.Fatalf("script does not start with the quoted system PING.EXE: %q", rc.scripts[0])
	}
	want := ping + ` -n 3 127.0.0.1 >nul & rmdir /s /q "` + r.InstallDir + `"`
	if rc.scripts[0] != want {
		t.Fatalf("scripts = %q, want %q", rc.scripts, want)
	}
	if len(res.Notes) == 0 || !exists(exe) {
		t.Fatalf("the running exe must be left to the script: %+v", res)
	}

	// A log file still open (by this process) cannot be deleted yet: the
	// script removes the data directory after exit, even when the running
	// binary is elsewhere.
	logFile := filepath.Join(r.DataDir, "logs", "stv2.log")
	writeFile(t, logFile, "open log")
	held, err := os.Open(logFile)
	if err != nil {
		t.Fatal(err)
	}
	rc.scripts = nil
	_, err = Uninstall(ctx, r, Options{Yes: true, Self: filepath.Join(root, "elsewhere", "stv2.exe")})
	held.Close()
	if err != nil {
		t.Fatal(err)
	}
	want = ping + ` -n 3 127.0.0.1 >nul & rmdir /s /q "` + r.DataDir + `"`
	if len(rc.scripts) != 1 || rc.scripts[0] != want {
		t.Fatalf("scripts = %q, want %q", rc.scripts, want)
	}
	if exists(exe) {
		t.Error("the installed exe was not deleted directly")
	}

	// Keeping a managed Syncthing inside the install dir: only our files go.
	if _, err := Install(ctx, r, Options{Yes: true, NoStart: true, Source: src, Self: src}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.ManagedSyncthingDir, "syncthing.exe"), "managed")
	rc.scripts = nil
	if _, err := Uninstall(ctx, r, Options{Yes: true, Self: exe}); err != nil {
		t.Fatal(err)
	}
	want = ping + ` -n 3 127.0.0.1 >nul & del /f /q "` + exe + `" "` + exe + `.old" 2>nul`
	if len(rc.scripts) != 1 || rc.scripts[0] != want {
		t.Fatalf("scripts = %q, want %q", rc.scripts, want)
	}

	// Paths cmd.exe cannot quote safely are refused.
	bad := r
	bad.InstallDir = filepath.Join(root, `100%`, "SyncThingV2")
	if _, err := Uninstall(ctx, bad, Options{Yes: true, Self: filepath.Join(bad.InstallDir, "stv2.exe")}); err == nil {
		t.Fatal("a path with %% was accepted for the cmd.exe script")
	}
}

func TestInteractiveUninstallAsksAboutSyncthing(t *testing.T) {
	root := t.TempDir()
	rc := &recorder{}
	r := rc.roots(testRoots(t, root))
	ctx := context.Background()
	self := filepath.Join(root, "elsewhere", brand.ExeName())
	src := fakeSource(t, root, "stv2")
	if _, err := Install(ctx, r, Options{Yes: true, NoStart: true, Source: src, Self: self}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.ManagedSyncthingDir, syncthingExe()), "managed")
	var asked []string
	res, err := Uninstall(ctx, r, Options{Self: self, Confirm: func(q string) bool { asked = append(asked, q); return true }})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{RemoveSyncthingQuestion}) || res.SyncthingKept || exists(r.ManagedSyncthingDir) {
		t.Fatalf("asked = %q, result = %+v", asked, res)
	}
}

func TestInstallRejectsBadRoots(t *testing.T) {
	ctx := context.Background()
	if _, err := Install(ctx, Roots{InstallDir: "relative", DataDir: t.TempDir(), Autostart: &autostart.Manager{}}, Options{}); err == nil {
		t.Error("relative InstallDir accepted")
	}
	if _, err := Uninstall(ctx, Roots{InstallDir: t.TempDir(), DataDir: t.TempDir()}, Options{}); err == nil {
		t.Error("missing autostart manager accepted")
	}
}

func TestPackagedLinuxBinaryIsUsedInPlace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("package-managed binaries are a Linux (.deb) case")
	}
	root := t.TempDir()
	rc := &recorder{}
	r := rc.roots(testRoots(t, root))
	src := filepath.Join(root, "usr", "bin", "stv2")
	writeFile(t, src, "packaged")
	res, err := Install(context.Background(), r, Options{Yes: true, NoStart: true, Source: src, Self: src})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exe != src || exists(InstalledExe(r)) || exists(filepath.Join(r.ApplicationsDir, "stv2.desktop")) {
		t.Fatalf("packaged install copied files: %+v", res)
	}
	if !enabled(t, r, autostart.Tray) {
		t.Fatal("tray autostart not set for the packaged binary")
	}
}

// ansiLnk is a minimal shell link whose LinkInfo LocalBasePath is target.
func ansiLnk(target string) []byte {
	le := binary.LittleEndian
	header := make([]byte, 0x4C)
	le.PutUint32(header, 0x4C)
	le.PutUint32(header[0x14:], 1<<1) // HasLinkInfo
	path := append([]byte(target), 0)
	info := make([]byte, 0x1C)
	le.PutUint32(info, uint32(0x1C+len(path)+1)) // LinkInfoSize
	le.PutUint32(info[4:], 0x1C)                 // LinkInfoHeaderSize
	le.PutUint32(info[8:], 1)                    // VolumeIDAndLocalBasePath
	le.PutUint32(info[0x10:], 0x1C)              // LocalBasePathOffset
	le.PutUint32(info[0x18:], uint32(0x1C+len(path)))
	return slices.Concat(header, info, path, []byte{0})
}

func TestLegacyMigration(t *testing.T) {
	root := t.TempDir()
	legacyExe := filepath.Join(root, "Programs", "Syncthing", "tray", "SyncthingTray.exe")
	// The unrelated Syncthing Tray project (Martchus) uses syncthingtray.exe.
	otherExe := filepath.Join(root, "Programs", "Syncthing Tray", "syncthingtray.exe")
	startup := filepath.Join(root, "Startup")
	lnk := filepath.Join(startup, "Syncthing Tray.lnk")
	lnkData := string(ansiLnk(legacyExe))
	writeFile(t, lnk, lnkData)
	writeFile(t, filepath.Join(startup, "Syncthing.lnk"), "syncthing autostart, not ours")
	procs := func() ([]Process, error) {
		return []Process{
			{PID: 4242, Name: "SyncthingTray.exe", Path: legacyExe},
			{PID: 7, Name: "explorer.exe", Path: `C:\Windows\explorer.exe`},
			{PID: 4243, Name: "SyncthingTray.exe", Path: legacyExe},
			{PID: 5000, Name: "syncthingtray.exe", Path: otherExe},
			{PID: 5001, Name: "SyncthingTray.exe"}, // path unreadable
		}, nil
	}
	r := Roots{StartupDir: startup, LegacyExe: legacyExe, Processes: procs}
	l := detectLegacy(r)
	if l.Shortcut != lnk || !slices.Equal(l.PIDs, []int{4242, 4243}) || !l.Found() {
		t.Fatalf("detectLegacy = %+v", l)
	}
	var killed []int
	if err := migrateLegacy(l, func(pid int) error { killed = append(killed, pid); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(killed, []int{4242, 4243}) {
		t.Errorf("terminated %v", killed)
	}
	if exists(lnk) || readFile(t, lnk+".disabled") != lnkData {
		t.Error("shortcut not renamed to .lnk.disabled")
	}
	if readFile(t, filepath.Join(startup, "Syncthing.lnk")) != "syncthing autostart, not ours" {
		t.Error("Syncthing.lnk was touched")
	}
	r.Processes = func() ([]Process, error) { return nil, nil }
	if l := detectLegacy(r); l.Found() {
		t.Errorf("legacy still detected after migration: %+v", l)
	}
	if l := detectLegacy(Roots{}); l.Found() {
		t.Error("detected legacy without any source")
	}

	// Only the prototype counts: the Syncthing Tray project's process and a
	// same-named Startup shortcut pointing elsewhere (or not a shell link)
	// are left alone.
	for _, data := range []string{string(ansiLnk(otherExe)), "synthetic text, not a shell link"} {
		writeFile(t, lnk, data)
		r.Processes = func() ([]Process, error) {
			return []Process{{PID: 5000, Name: "syncthingtray.exe", Path: otherExe}, {PID: 5002, Name: "SyncthingTray.exe", Path: otherExe}}, nil
		}
		if l := detectLegacy(r); l.Found() {
			t.Errorf("the Syncthing Tray project was taken for the legacy tray: %+v", l)
		}
	}
	// Without a known prototype location nothing is detected at all.
	writeFile(t, lnk, lnkData)
	r.LegacyExe = ""
	r.Processes = procs
	if l := detectLegacy(r); l.Found() {
		t.Errorf("detected legacy without LegacyExe: %+v", l)
	}
	err := migrateLegacy(Legacy{PIDs: []int{1}}, func(int) error { return errors.New("denied") })
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("terminate failure not reported: %v", err)
	}
}

func TestInstallMigratesLegacyOnConsent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the legacy prototype tray existed on Windows only")
	}
	root := t.TempDir()
	r := testRoots(t, root)
	rc := &recorder{procs: []Process{{PID: 4242, Name: "SyncthingTray.exe", Path: r.LegacyExe}}}
	r = rc.roots(r)
	lnk := filepath.Join(r.StartupDir, "Syncthing Tray.lnk")
	writeFile(t, lnk, string(ansiLnk(r.LegacyExe)))
	self := filepath.Join(root, "elsewhere", "stv2.exe")
	src := fakeSource(t, root, "stv2")

	// --yes leaves the question to the dashboard notice.
	res, err := Install(context.Background(), r, Options{Yes: true, NoStart: true, Source: src, Self: self})
	if err != nil {
		t.Fatal(err)
	}
	if res.LegacyMigrated || len(rc.killed) != 0 || !exists(lnk) || len(res.Notes) == 0 {
		t.Fatalf("--yes migrated without consent: %+v", res)
	}

	// Interactive "no" is recorded and changes nothing.
	asked := 0
	res, err = Install(context.Background(), r, Options{NoStart: true, Source: src, Self: self, Confirm: func(q string) bool {
		if q == LegacyQuestion {
			asked++
		}
		return false
	}})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 || res.LegacyMigrated || len(rc.killed) != 0 || !exists(lnk) || exists(lnk+".disabled") {
		t.Fatalf("after no: asked %d, result %+v, killed %v", asked, res, rc.killed)
	}
	st, err := prefs.Open(r.DataDir)
	if err != nil || !st.Get().LegacyAsked {
		t.Fatalf("LegacyAsked not recorded after no: %v", err)
	}

	// Once answered, neither an interactive nor a --yes install asks again or
	// promises a dashboard offer.
	never := func(q string) bool {
		if q == LegacyQuestion {
			t.Fatal("legacy question asked again after it was answered")
		}
		return false
	}
	for _, o := range []Options{{Confirm: never}, {Yes: true, Confirm: never}} {
		o.NoStart, o.Source, o.Self = true, src, self
		res, err = Install(context.Background(), r, o)
		if err != nil {
			t.Fatal(err)
		}
		if res.LegacyMigrated || len(rc.killed) != 0 || !exists(lnk) || slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "older Syncthing tray") }) {
			t.Fatalf("answered question acted on again: %+v, killed %v", res, rc.killed)
		}
	}

	// Interactive "yes" (on a fresh answer).
	if err := st.Update(func(p *prefs.Prefs) error { p.LegacyAsked = false; return nil }); err != nil {
		t.Fatal(err)
	}
	res, err = Install(context.Background(), r, Options{NoStart: true, Source: src, Self: self, Confirm: func(q string) bool { return q == LegacyQuestion }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LegacyMigrated || !slices.Equal(rc.killed, []int{4242}) || exists(lnk) || !exists(lnk+".disabled") {
		t.Fatalf("result = %+v, killed = %v", res, rc.killed)
	}
	st, err = prefs.Open(r.DataDir)
	if err != nil || !st.Get().LegacyAsked {
		t.Fatalf("LegacyAsked not recorded: %v", err)
	}
	if _, err := Uninstall(context.Background(), r, Options{Yes: true, RemoveSyncthing: true, Self: self}); err != nil {
		t.Fatal(err)
	}
	if !exists(lnk + ".disabled") {
		t.Error("uninstall removed the disabled legacy shortcut")
	}
}

func TestFirewallRule(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "syncthing", "syncthing.exe")
	var calls []string
	codes := map[string]int{}
	fw := firewall{
		elevated: func() bool { return true },
		netsh: func(_ context.Context, args string) ([]byte, int, error) {
			calls = append(calls, args)
			for prefix, code := range codes {
				if strings.HasPrefix(args, prefix) {
					return []byte("netsh output"), code, nil
				}
			}
			return nil, 0, nil
		},
		relaunch: func(context.Context, []string) error { t.Fatal("relaunch while elevated"); return nil },
	}
	codes["advfirewall firewall delete"] = 1 // "No rules match" is fine
	if err := fw.allow(context.Background(), bin); err != nil {
		t.Fatal(err)
	}
	rule := func(proto string) string {
		return `advfirewall firewall add rule name="SyncThing V2 - Syncthing" dir=in action=allow program="` + bin +
			`" protocol=` + proto + ` localport=22000 remoteip=100.64.0.0/10,LocalSubnet enable=yes`
	}
	want := []string{`advfirewall firewall delete rule name="SyncThing V2 - Syncthing"`, rule("TCP"), rule("UDP")}
	if !slices.Equal(calls, want) {
		t.Fatalf("netsh calls:\n%q\nwant\n%q", calls, want)
	}

	// A failing add is reported.
	calls, codes["advfirewall firewall add"] = nil, 1
	if err := fw.allow(context.Background(), bin); err == nil || !strings.Contains(err.Error(), "netsh output") {
		t.Fatalf("failed add: %v", err)
	}

	// Not elevated: re-launch with the elevated arguments, no netsh.
	var relaunched []string
	calls = nil
	fw.elevated = func() bool { return false }
	fw.relaunch = func(_ context.Context, args []string) error { relaunched = args; return ErrElevationDeclined }
	if err := fw.allow(context.Background(), bin); !errors.Is(err, ErrElevationDeclined) {
		t.Fatalf("declined elevation: %v", err)
	}
	if !slices.Equal(relaunched, []string{"firewall", "allow", "--bin", bin, "--elevated"}) || len(calls) != 0 {
		t.Fatalf("relaunched %q, netsh %q", relaunched, calls)
	}

	// The re-launched child that is still not elevated fails instead of
	// starting another child.
	child := fw
	child.child = true
	child.relaunch = func(context.Context, []string) error { t.Fatal("the elevated child re-launched itself"); return nil }
	if err := child.allow(context.Background(), bin); !errors.Is(err, ErrNotElevated) || len(calls) != 0 {
		t.Fatalf("non-elevated child: %v, netsh %q", err, calls)
	}
	// Elevated, the child adds the rule like the parent would.
	child.elevated = func() bool { return true }
	delete(codes, "advfirewall firewall add")
	if err := child.allow(context.Background(), bin); err != nil || !slices.Equal(calls, want) {
		t.Fatalf("elevated child: %v, netsh %q", err, calls)
	}
	calls = nil

	// Unsafe or relative paths never reach netsh.
	for _, b := range []string{"syncthing.exe", filepath.Join(t.TempDir(), `a"b`, "syncthing.exe"), filepath.Join(t.TempDir(), "%PATH%", "syncthing.exe")} {
		if err := fw.allow(context.Background(), b); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	if len(calls) != 0 || relaunched == nil {
		t.Errorf("netsh ran for a rejected path: %q", calls)
	}

	// HasRule looks up our rule name only.
	for code, want := range map[int]bool{0: true, 1: false} {
		calls = nil
		fw.netsh = func(_ context.Context, args string) ([]byte, int, error) {
			calls = append(calls, args)
			return nil, code, nil
		}
		got, err := fw.hasRule(context.Background())
		if err != nil || got != want {
			t.Errorf("exit %d: hasRule = %v, %v", code, got, err)
		}
		if !slices.Equal(calls, []string{`advfirewall firewall show rule name="SyncThing V2 - Syncthing"`}) {
			t.Errorf("show call = %q", calls)
		}
	}
	fw.netsh = func(context.Context, string) ([]byte, int, error) { return nil, 5, nil }
	if _, err := fw.hasRule(context.Background()); err == nil {
		t.Error("unexpected exit code accepted")
	}
}

func TestHasFirewallRulePlatform(t *testing.T) {
	has, err := HasFirewallRule(context.Background())
	if runtime.GOOS != "windows" {
		if !errors.Is(err, ErrFirewallUnsupported) || has {
			t.Fatalf("HasFirewallRule = %v, %v; want ErrFirewallUnsupported", has, err)
		}
		if !errors.Is(AllowFirewall(context.Background(), "/usr/bin/syncthing"), ErrFirewallUnsupported) ||
			!errors.Is(AllowFirewallElevated(context.Background(), "/usr/bin/syncthing"), ErrFirewallUnsupported) {
			t.Fatal("AllowFirewall must be unsupported off Windows")
		}
		return
	}
	// Read-only query of the real firewall: it must answer without error.
	if err != nil {
		t.Fatalf("HasFirewallRule: %v", err)
	}
}

func TestRemoveTreeExcept(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	keep := filepath.Join(data, "syncthing")
	writeFile(t, filepath.Join(keep, "syncthing"), "bin")
	writeFile(t, filepath.Join(data, "prefs.json"), "{}")
	writeFile(t, filepath.Join(data, "logs", "a.log"), "x")
	if err := removeTreeExcept(data, keep); err != nil {
		t.Fatal(err)
	}
	if got := listTree(t, data); !slices.Equal(got, []string{"syncthing", "syncthing/syncthing"}) {
		t.Fatalf("after removeTreeExcept: %q", got)
	}
	if err := removeTreeExcept(data, ""); err != nil || exists(data) {
		t.Fatalf("full removal: %v", err)
	}
	if err := removeTreeExcept(data, keep); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	// A keep path outside the tree does not protect anything inside it.
	writeFile(t, filepath.Join(data, "x"), "x")
	if err := removeTreeExcept(data, filepath.Join(dir, "other")); err != nil || exists(data) {
		t.Fatalf("outside keep: %v", err)
	}
}

func TestWithin(t *testing.T) {
	base := filepath.Join(t.TempDir(), "a")
	cases := []struct {
		p    string
		want bool
	}{
		{base, true},
		{filepath.Join(base, "b", "c"), true},
		{base + "b", false},
		{filepath.Dir(base), false},
		{filepath.Join(base, "..", "x"), false},
	}
	for _, c := range cases {
		if got := within(c.p, base); got != c.want {
			t.Errorf("within(%q, %q) = %v", c.p, base, got)
		}
	}
	if runtime.GOOS == "windows" && !within(strings.ToUpper(filepath.Join(base, "b")), base) {
		t.Error("within must ignore case on Windows")
	}
}

func TestStopRunningSkipsMissingInstance(t *testing.T) {
	rc := &recorder{}
	r := rc.roots(Roots{DataDir: t.TempDir()})
	stopRunning(context.Background(), r, Options{Log: &bytes.Buffer{}})
	if len(rc.signals) != 0 {
		t.Fatalf("signalled without instance.json: %q", rc.signals)
	}
	// Our own PID is never asked to quit.
	if err := single.WriteInstance(r.DataDir, single.Instance{Port: 1, PID: os.Getpid(), Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	stopRunning(context.Background(), r, Options{Log: &bytes.Buffer{}})
	if len(rc.signals) != 0 {
		t.Fatalf("signalled ourselves: %q", rc.signals)
	}
}

// TestSelfDeleteCommandLine pins the cmd.exe switches: /d (no AutoRun),
// /v:off (no delayed expansion, whatever the DelayedExpansion policy says)
// and /s with one outer pair of quotes (so a script starting with a quoted
// program path keeps its quotes). On Windows it also shows why /v:off
// matters: with delayed expansion on, cmd.exe expands !VAR!, which quoting
// does not prevent.
func TestSelfDeleteCommandLine(t *testing.T) {
	const script = `"C:\Windows\system32\PING.EXE" -n 3 127.0.0.1 >nul & rmdir /s /q "C:\a!b!c"`
	want := `"C:\Windows\system32\cmd.exe" /d /v:off /s /c "` + script + `"`
	if got := selfDeleteCommandLine(`C:\Windows\system32\cmd.exe`, script); got != want {
		t.Fatalf("command line = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		return
	}
	comspec := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	for flag, want := range map[string]string{"/v:off": "x!OS!y", "/v:on": "xWindows_NTy"} {
		out, err := exec.Command(comspec, "/d", flag, "/c", "echo x!OS!y").Output()
		if err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("%s: echo printed %s, want %s", flag, got, want)
		}
	}
}

// TestSelfDeleteRuns runs the real detached cmd.exe on a temporary
// directory whose name needs quoting, and waits for it to disappear.
func TestSelfDeleteRuns(t *testing.T) {
	if runtime.GOOS != "windows" {
		if err := selfDelete("true"); err == nil {
			t.Fatal("selfDelete must refuse to run off Windows")
		}
		return
	}
	// '!' must stay literal: with delayed expansion on, "!OS!" would become
	// "Windows_NT" and rmdir would target another path.
	dir := filepath.Join(t.TempDir(), "Program Files & Co !OS!", "SyncThingV2")
	writeFile(t, filepath.Join(dir, "stv2.exe"), "fake")
	writeFile(t, filepath.Join(dir, "sub", "file.txt"), "fake")
	ping := filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE")
	if err := selfDelete(`"` + ping + `" -n 2 127.0.0.1 >nul & rmdir /s /q "` + dir + `"`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for exists(dir) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists", dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !exists(filepath.Dir(dir)) {
		t.Fatal("the script removed more than the named directory")
	}
}

func TestCheckLoginPath(t *testing.T) {
	for _, c := range []struct {
		exe string
		ok  bool
	}{
		{"/Applications/SyncThing V2.app/Contents/MacOS/stv2", true},
		{"/Users/example/Applications/SyncThing V2.app/Contents/MacOS/stv2", true},
		{"/Users/example/Library/Application Support/SyncThingV2/bin/stv2", true},
		{"/Volumes/SyncThing V2/SyncThing V2.app/Contents/MacOS/stv2", false},
		{"/private/var/folders/xy/abc/T/AppTranslocation/0A1B2C3D/d/SyncThing V2.app/Contents/MacOS/stv2", false},
		{"/Applications/../Volumes/Image/SyncThing V2.app/Contents/MacOS/stv2", false},
	} {
		if err := CheckLoginPath(c.exe); (err == nil) != c.ok {
			t.Errorf("CheckLoginPath(%q) = %v, want ok %v", c.exe, err, c.ok)
		}
	}
}

// sameFile reports whether a and b name the same existing file.
func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}
