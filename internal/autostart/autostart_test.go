package autostart

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// ---------------------------------------------------------------------------
// Shell links

// lnkSpec describes a synthetic .lnk file.
type lnkSpec struct {
	ansiPath    string // LinkInfo LocalBasePath (ANSI)
	unicodePath string // LinkInfo LocalBasePathUnicode ("" = ANSI-only header)
	relative    string // StringData RELATIVE_PATH
	args        string // StringData COMMAND_LINE_ARGUMENTS
	name        string // StringData NAME_STRING
}

func buildLnk(s lnkSpec) []byte {
	le := binary.LittleEndian
	flags := uint32(lnkIsUnicode | lnkHasIDList)
	if s.ansiPath != "" || s.unicodePath != "" {
		flags |= lnkHasLinkInfo
	}
	if s.name != "" {
		flags |= lnkHasName
	}
	if s.relative != "" {
		flags |= lnkHasRelativePath
	}
	if s.args != "" {
		flags |= lnkHasArguments
	}
	var b bytes.Buffer
	header := make([]byte, lnkHeaderSize)
	le.PutUint32(header, lnkHeaderSize)
	copy(header[4:], []byte{0x01, 0x14, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46})
	le.PutUint32(header[0x14:], flags)
	b.Write(header)

	idl := []byte{4, 0, 0xAA, 0xBB, 0, 0} // one 4-byte item and the terminator
	_ = binary.Write(&b, le, uint16(len(idl)))
	b.Write(idl)

	if flags&lnkHasLinkInfo != 0 {
		hdr := 0x1C
		if s.unicodePath != "" {
			hdr = 0x24
		}
		vol := []byte{0x11, 0, 0, 0, 3, 0, 0, 0, 0x78, 0x56, 0x34, 0x12, 0x10, 0, 0, 0, 0}
		ansi := append([]byte(s.ansiPath), 0)
		suffix := []byte{0}
		var ubase, usuf []byte
		if s.unicodePath != "" {
			ubase = utf16Bytes(s.unicodePath + "\x00")
			usuf = []byte{0, 0}
		}
		off := hdr
		volOff := off
		off += len(vol)
		baseOff := off
		off += len(ansi)
		sufOff := off
		off += len(suffix)
		ubaseOff := off
		off += len(ubase)
		usufOff := off
		off += len(usuf)
		fields := []uint32{uint32(off), uint32(hdr), linkInfoLocalPath, uint32(volOff), uint32(baseOff), 0, uint32(sufOff)}
		if s.unicodePath != "" {
			fields = append(fields, uint32(ubaseOff), uint32(usufOff))
		}
		for _, f := range fields {
			_ = binary.Write(&b, le, f)
		}
		b.Write(vol)
		b.Write(ansi)
		b.Write(suffix)
		b.Write(ubase)
		b.Write(usuf)
	}
	for _, str := range []string{s.name, s.relative, s.args} {
		if str == "" {
			continue
		}
		u := utf16.Encode([]rune(str))
		_ = binary.Write(&b, le, uint16(len(u)))
		b.Write(utf16Bytes(str))
	}
	b.Write([]byte{0, 0, 0, 0}) // terminal block
	return b.Bytes()
}

func utf16Bytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(out[2*i:], c)
	}
	return out
}

func TestParseShellLink(t *testing.T) {
	cases := []struct {
		name string
		spec lnkSpec
		want ShellLink
	}{
		{"ansi link info", lnkSpec{ansiPath: `C:\Tools\syncthing.exe`, args: "serve --no-console --no-browser", name: "Syncthing"},
			ShellLink{Target: `C:\Tools\syncthing.exe`, Args: "serve --no-console --no-browser"}},
		{"unicode preferred", lnkSpec{ansiPath: `C:\WRONG\other.exe`, unicodePath: `C:\Users\Zoë\Síncronía\syncthing.exe`},
			ShellLink{Target: `C:\Users\Zoë\Síncronía\syncthing.exe`}},
		{"relative path only", lnkSpec{relative: `..\..\Tools\syncthing.exe`, args: "-no-console"},
			ShellLink{Target: `..\..\Tools\syncthing.exe`, Args: "-no-console"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseShellLink(buildLnk(c.spec))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("ParseShellLink = %+v, want %+v", got, c.want)
			}
		})
	}
	valid := buildLnk(lnkSpec{ansiPath: `C:\x\syncthing.exe`, args: "serve"})
	for name, data := range map[string][]byte{
		"empty":     nil,
		"bad size":  append([]byte{0x4D, 0, 0, 0}, make([]byte, 100)...),
		"truncated": valid[:len(valid)-12],
		"no target": buildLnk(lnkSpec{args: "serve"}),
	} {
		if _, err := ParseShellLink(data); err == nil {
			t.Errorf("%s: ParseShellLink accepted it", name)
		}
	}
}

func TestReferencesSyncthing(t *testing.T) {
	yes := []string{
		`"C:\Tools\syncthing.exe" serve --no-console`,
		`C:\Tools\Syncthing.EXE -no-console -no-browser`,
		`cmd /c start "" "C:\Program Files\Syncthing\syncthing.exe"`,
		`C:\Tools\syncthing serve`,
		`syncthing`,
	}
	no := []string{
		`"C:\Users\u\AppData\Local\Programs\SyncThingV2\stv2.exe" --background`,
		`"C:\Programs\SyncthingTray\SyncthingTray.exe"`,
		`"C:\Program Files\SyncTrayzor\SyncTrayzor.exe" --minimized`,
		``,
	}
	for _, c := range yes {
		if !ReferencesSyncthing(c) {
			t.Errorf("ReferencesSyncthing(%q) = false", c)
		}
	}
	for _, c := range no {
		if ReferencesSyncthing(c) {
			t.Errorf("ReferencesSyncthing(%q) = true", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Renderers

func TestDesktopExec(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/stv2":      "/usr/bin/stv2",
		"--background":       "--background",
		"/home/a b/stv2":     `"/home/a b/stv2"`,
		`/opt/$x/"q"`:        `"/opt/\\$x/\\"q\\""`,
		"100%":               "100%%",
		`back\slash`:         `"back\\\\slash"`,
		"":                   `""`,
		"/home/u/`cmd`/stv2": "\"/home/u/\\\\`cmd\\\\`/stv2\"",
	}
	for in, want := range cases {
		if got := desktopExecArg(in); got != want {
			t.Errorf("desktopExecArg(%q) = %s, want %s", in, got, want)
		}
	}
	d := renderDesktop("Name", "Line one\nline two", []string{"/home/a b/stv2", "--background"})
	kv := desktopEntry(d)
	if kv["Exec"] != `"/home/a b/stv2" --background` || kv["Type"] != "Application" || kv["Comment"] != `Line one\nline two` {
		t.Errorf("renderDesktop keys = %v", kv)
	}
	if kv["X-GNOME-Autostart-enabled"] != "true" || kv["Icon"] != brand.BinaryName {
		t.Errorf("renderDesktop keys = %v", kv)
	}
}

func TestDesktopActive(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]bool{
		write("on.desktop", "[Desktop Entry]\nExec=x\n"):                                           true,
		write("hidden.desktop", "[Desktop Entry]\nHidden=true\n"):                                  false,
		write("off.desktop", "[Desktop Entry]\nX-GNOME-Autostart-enabled=false\n"):                 false,
		write("other-group.desktop", "[Desktop Entry]\nExec=x\n[Desktop Action a]\nHidden=true\n"): true,
		filepath.Join(dir, "missing.desktop"):                                                      false,
	}
	for p, want := range cases {
		if got, err := desktopActive(p); err != nil || got != want {
			t.Errorf("desktopActive(%s) = %v, %v; want %v", filepath.Base(p), got, err, want)
		}
	}
}

func TestRenderUnit(t *testing.T) {
	u := string(renderUnit([]string{"/home/a b/.local/share/syncthing-v2/syncthing/syncthing", "serve", "--no-browser", "--no-restart", "50%$"}))
	for _, want := range []string{
		`ExecStart="/home/a b/.local/share/syncthing-v2/syncthing/syncthing" serve --no-browser --no-restart 50%%$$` + "\n",
		"Restart=on-failure\n",
		"SuccessExitStatus=3 4\n",
		"RestartForceExitStatus=3 4\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if got := unitExec([]byte(u)); got != "/home/a b/.local/share/syncthing-v2/syncthing/syncthing" {
		t.Errorf("unitExec(own unit) = %q", got)
	}
	distro := "[Unit]\nDescription=x\n[Service]\nExecStart=-/usr/bin/syncthing serve --no-browser --no-restart --logflags=0\n"
	if got := unitExec([]byte(distro)); got != "/usr/bin/syncthing" {
		t.Errorf("unitExec(distro) = %q", got)
	}
	for in, want := range map[string]string{
		`ExecStart="/opt/a \"b\"/100%%/syncthing" serve`: `/opt/a "b"/100%/syncthing`,
		`ExecStart=/opt/a\ b/syncthing serve`:            `/opt/a b/syncthing`,
		`ExecStart="/opt/unterminated`:                   "",
	} {
		if got := unitExec([]byte(in)); got != want {
			t.Errorf("unitExec(%s) = %q, want %q", in, got, want)
		}
	}
	if got := unitExec([]byte("[Service]\nType=simple\n")); got != "" {
		t.Errorf("unitExec without ExecStart = %q", got)
	}
}

func TestRenderPlist(t *testing.T) {
	argv := []string{"/Users/a&b/Applications/SyncThing V2.app/Contents/MacOS/stv2", "--background"}
	p := renderPlist(launchdLabel(Tray), argv, false, "")
	vals, err := plistTopLevel(p)
	if err != nil {
		t.Fatal(err)
	}
	if vals["Label"] != brand.BundleID || vals["ProgramArguments"] != strings.Join(argv, "\x00") || vals["ProcessType"] != "Interactive" {
		t.Errorf("tray plist values = %q", vals)
	}
	d := renderPlist(launchdLabel(Syncthing), []string{"/x/syncthing", "serve"}, true, "/Users/u/Library/Logs/SyncThingV2/syncthing.log")
	vals, err = plistTopLevel(d)
	if err != nil {
		t.Fatal(err)
	}
	if vals["Label"] != brand.BundleID+".syncthing" || vals["StandardOutPath"] != "/Users/u/Library/Logs/SyncThingV2/syncthing.log" || vals["ProcessType"] != "Background" {
		t.Errorf("syncthing plist values = %q", vals)
	}
	if !bytes.Contains(d, []byte("<key>KeepAlive</key>\n\t<true/>")) || !bytes.Contains(d, []byte("<key>RunAtLoad</key>\n\t<true/>")) {
		t.Errorf("syncthing plist lacks KeepAlive/RunAtLoad:\n%s", d)
	}
	if _, err := plistTopLevel([]byte("<plist><dict><key>Label</key>")); err == nil {
		t.Error("plistTopLevel accepted truncated XML")
	}
}

// ---------------------------------------------------------------------------
// launchd

type callLog struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error // call prefix -> error
}

func (c *callLog) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call := strings.Join(append([]string{name}, args...), " ")
	c.calls = append(c.calls, call)
	for prefix, err := range c.fail {
		if strings.HasPrefix(call, prefix) {
			return nil, err
		}
	}
	return nil, nil
}

func TestLaunchdRoundTrip(t *testing.T) {
	root := t.TempDir()
	log := &callLog{}
	r := Roots{
		LaunchAgents: filepath.Join(root, "Library", "LaunchAgents"),
		LogDir:       filepath.Join(root, "Library", "Logs", "SyncThingV2"),
		UID:          501,
		Run:          log.run,
	}
	m := &Manager{impl: newLaunchd(r)}
	bin := filepath.Join(root, "SyncThing V2.app", "Contents", "MacOS", "stv2")
	st := filepath.Join(root, "syncthing", "syncthing")

	roundTrip(t, m, bin, st)
	// Turning Syncthing off boots the loaded KeepAlive job out before the
	// plist goes (the fake reports it loaded both times); the tray is not a
	// KeepAlive job and needs no launchctl call.
	service := "gui/501/" + brand.BundleID + ".syncthing"
	offCalls := []string{"launchctl print " + service, "launchctl bootout " + service}
	if want := append(append([]string{}, offCalls...), offCalls...); !reflect.DeepEqual(log.calls, want) {
		t.Errorf("launchctl calls while turning off = %q, want %q", log.calls, want)
	}
	log.calls = nil

	// Contents.
	must(t, m.Set(Syncthing, true, st, nil))
	data, err := os.ReadFile(filepath.Join(r.LaunchAgents, brand.BundleID+".syncthing.plist"))
	if err != nil {
		t.Fatal(err)
	}
	vals, err := plistTopLevel(data)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{st, "serve", "--no-browser", "--no-restart"}, "\x00"); vals["ProgramArguments"] != want {
		t.Errorf("ProgramArguments = %q, want %q", vals["ProgramArguments"], want)
	}
	if vals["StandardOutPath"] != filepath.Join(r.LogDir, "syncthing.log") {
		t.Errorf("log path = %q", vals["StandardOutPath"])
	}
	if fi, err := os.Stat(r.LogDir); err != nil || !fi.IsDir() {
		t.Errorf("log dir not created: %v", err)
	}

	// Starting through launchd: bootstrap, then kickstart when already loaded.
	if ok, err := m.StartService(Syncthing); !ok || err != nil {
		t.Errorf("StartService = %v, %v", ok, err)
	}
	log.fail = map[string]error{"launchctl bootstrap": errors.New("service already loaded")}
	if ok, err := m.StartService(Syncthing); !ok || err != nil {
		t.Errorf("StartService (loaded) = %v, %v", ok, err)
	}
	wantCalls := []string{
		"launchctl bootstrap gui/501 " + filepath.Join(r.LaunchAgents, brand.BundleID+".syncthing.plist"),
		"launchctl bootstrap gui/501 " + filepath.Join(r.LaunchAgents, brand.BundleID+".syncthing.plist"),
		"launchctl kickstart gui/501/" + brand.BundleID + ".syncthing",
	}
	if !reflect.DeepEqual(log.calls, wantCalls) {
		t.Errorf("launchctl calls = %q, want %q", log.calls, wantCalls)
	}
	if ok, _ := m.StartService(Tray); ok {
		t.Error("StartService(Tray) claimed a service")
	}

	// Foreign LaunchAgents.
	must(t, os.WriteFile(filepath.Join(r.LaunchAgents, "com.example.other.plist"), []byte("x"), 0o644))
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("Existing = %q with only our own and unrelated agents", desc)
	}
	must(t, os.WriteFile(filepath.Join(r.LaunchAgents, "homebrew.mxcl.syncthing.plist"), []byte("x"), 0o644))
	if desc, ok := m.Existing(Syncthing); !ok || desc != "LaunchAgent homebrew.mxcl.syncthing.plist" {
		t.Errorf("Existing = %q, %v", desc, ok)
	}
	if _, ok := m.Existing(Tray); ok {
		t.Error("Existing(Tray) reported an entry")
	}

	// A plist with our name but another label is not ours.
	must(t, os.WriteFile(filepath.Join(r.LaunchAgents, brand.BundleID+".plist"),
		renderPlist("com.example.impostor", []string{"/bin/true"}, false, ""), 0o644))
	if on, err := m.Enabled(Tray); err != nil || on {
		t.Errorf("Enabled with a foreign label = %v, %v", on, err)
	}
}

func TestLaunchdDisableUnloads(t *testing.T) {
	root := t.TempDir()
	st := filepath.Join(root, "syncthing", "syncthing")
	service := "gui/501/" + brand.BundleID + ".syncthing"
	setup := func(t *testing.T, fail map[string]error, run bool) (*Manager, *callLog, string) {
		t.Helper()
		log := &callLog{fail: fail}
		r := Roots{
			LaunchAgents: filepath.Join(t.TempDir(), "LaunchAgents"),
			LogDir:       filepath.Join(t.TempDir(), "Logs"),
			UID:          501,
		}
		if run {
			r.Run = log.run
		}
		m := &Manager{impl: newLaunchd(r)}
		must(t, m.Set(Syncthing, true, st, nil))
		return m, log, filepath.Join(r.LaunchAgents, brand.BundleID+".syncthing.plist")
	}

	t.Run("not loaded", func(t *testing.T) {
		m, log, plist := setup(t, map[string]error{"launchctl print": errors.New("Could not find service")}, true)
		if err := m.Set(Syncthing, false, "", nil); err != nil {
			t.Fatalf("Set(false) = %v", err)
		}
		if exists(plist) {
			t.Error("plist left behind")
		}
		if want := []string{"launchctl print " + service}; !reflect.DeepEqual(log.calls, want) {
			t.Errorf("calls = %q, want %q (no bootout for a job that is not loaded)", log.calls, want)
		}
	})

	t.Run("bootout fails", func(t *testing.T) {
		m, log, plist := setup(t, map[string]error{"launchctl bootout": errors.New("Boot-out failed: 1: Operation not permitted")}, true)
		err := m.Set(Syncthing, false, "", nil)
		if err == nil || !strings.Contains(err.Error(), "until logout") || !strings.Contains(err.Error(), "Operation not permitted") {
			t.Errorf("Set(false) = %v, want an error saying launchd keeps Syncthing running", err)
		}
		if exists(plist) {
			t.Error("plist kept although the login entry must go even when bootout fails")
		}
		if want := []string{"launchctl print " + service, "launchctl bootout " + service}; !reflect.DeepEqual(log.calls, want) {
			t.Errorf("calls = %q, want %q", log.calls, want)
		}
	})

	t.Run("without launchctl", func(t *testing.T) {
		m, log, plist := setup(t, nil, false)
		if err := m.Set(Syncthing, false, "", nil); err != nil {
			t.Fatalf("Set(false) = %v", err)
		}
		if exists(plist) || len(log.calls) != 0 {
			t.Errorf("plist exists = %v, calls = %q", exists(plist), log.calls)
		}
	})
}

// ---------------------------------------------------------------------------
// systemd / XDG

// fakeSystemctl imitates `systemctl` for the XDG tests. Enabling a unit
// creates its wants link under configHome, as systemctl does.
type fakeSystemctl struct {
	mu          sync.Mutex
	configHome  string
	available   bool
	enabled     map[string]bool // user units
	systemUnits map[string]bool
	calls       []string
}

func newFakeSystemctl(configHome string) *fakeSystemctl {
	return &fakeSystemctl{configHome: configHome, available: true, enabled: map[string]bool{}, systemUnits: map[string]bool{}}
}

func (f *fakeSystemctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if name != "systemctl" || !f.available {
		return nil, errors.New("exec: not available")
	}
	wants := func(unit string) string {
		return filepath.Join(f.configHome, "systemd", "user", "default.target.wants", unit)
	}
	a := strings.Join(args, " ")
	switch {
	case a == "--user show-environment":
		return []byte("PATH=/usr/bin\n"), nil
	case a == "--user daemon-reload":
		return nil, nil
	case strings.HasPrefix(a, "--user enable "):
		unit := args[len(args)-1]
		f.enabled[unit] = true
		if err := os.MkdirAll(filepath.Dir(wants(unit)), 0o755); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(wants(unit), nil, 0o644)
	case strings.HasPrefix(a, "--user disable "):
		unit := args[len(args)-1]
		delete(f.enabled, unit)
		_ = os.Remove(wants(unit))
		return nil, nil
	case strings.HasPrefix(a, "--user is-enabled "):
		if f.enabled[args[len(args)-1]] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errors.New("exit status 1")
	case strings.HasPrefix(a, "is-enabled "):
		if f.systemUnits[args[len(args)-1]] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errors.New("exit status 1")
	case strings.HasPrefix(a, "--user list-unit-files "):
		var b strings.Builder
		for u := range f.enabled {
			if strings.HasPrefix(u, "syncthing") {
				fmt.Fprintf(&b, "%s enabled enabled\n", u)
			}
		}
		return []byte(b.String()), nil
	case strings.HasPrefix(a, "--user start "):
		return nil, nil
	}
	return nil, fmt.Errorf("fake systemctl: unexpected %q", a)
}

func (f *fakeSystemctl) took(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func xdgRoots(t *testing.T) (Roots, string) {
	root := t.TempDir()
	return Roots{
		ConfigHome: filepath.Join(root, ".config"),
		UnitDirs:   []string{filepath.Join(root, "usr", "lib", "systemd", "user")},
		User:       "alice",
	}, root
}

func TestXDGWithoutSystemd(t *testing.T) {
	r, root := xdgRoots(t)
	m := &Manager{impl: newXDG(r)}
	bin := filepath.Join(root, "bin dir", "stv2")
	st := filepath.Join(root, ".local", "share", "syncthing-v2", "syncthing", "syncthing")
	roundTrip(t, m, bin, st)

	must(t, m.Set(Tray, true, bin, nil))
	kv := desktopEntry(readFile(t, filepath.Join(r.ConfigHome, "autostart", "stv2.desktop")))
	if kv["Exec"] != desktopExecArg(bin)+" --background" {
		t.Errorf("tray Exec = %q", kv["Exec"])
	}
	must(t, m.Set(Syncthing, true, st, nil))
	kv = desktopEntry(readFile(t, filepath.Join(r.ConfigHome, "autostart", "stv2-syncthing.desktop")))
	if kv["Exec"] != desktopExecArg(st)+" serve --no-browser" {
		t.Errorf("syncthing Exec = %q (no --no-restart without a supervisor)", kv["Exec"])
	}
	if ok, _ := m.StartService(Syncthing); ok {
		t.Error("StartService claimed a service without systemd")
	}
	// Hidden by the desktop environment's settings -> not enabled.
	must(t, os.WriteFile(filepath.Join(r.ConfigHome, "autostart", "stv2.desktop"), []byte("[Desktop Entry]\nExec=x\nHidden=true\n"), 0o644))
	if on, _ := m.Enabled(Tray); on {
		t.Error("a hidden entry counts as enabled")
	}
	// Foreign XDG entries.
	must(t, os.WriteFile(filepath.Join(r.ConfigHome, "autostart", "syncthing-start.desktop"), []byte("[Desktop Entry]\nExec=syncthing serve\n"), 0o644))
	if desc, ok := m.Existing(Syncthing); !ok || desc != "autostart entry syncthing-start.desktop" {
		t.Errorf("Existing = %q, %v", desc, ok)
	}
}

func TestXDGOwnUnit(t *testing.T) {
	r, root := xdgRoots(t)
	sd := newFakeSystemctl(r.ConfigHome)
	r.Run = sd.run
	m := &Manager{impl: newXDG(r)}
	st := filepath.Join(root, "managed dir", "syncthing")

	// A stale fallback entry is replaced by the unit.
	must(t, os.MkdirAll(filepath.Join(r.ConfigHome, "autostart"), 0o755))
	must(t, os.WriteFile(filepath.Join(r.ConfigHome, "autostart", "stv2-syncthing.desktop"), []byte("[Desktop Entry]\nExec=x\n"), 0o644))

	must(t, m.Set(Syncthing, true, st, nil))
	unitPath := filepath.Join(r.ConfigHome, "systemd", "user", "stv2-syncthing.service")
	if got := unitExec(readFile(t, unitPath)); got != st {
		t.Errorf("unit ExecStart program = %q", got)
	}
	if !strings.Contains(string(readFile(t, unitPath)), " serve --no-browser --no-restart\n") {
		t.Errorf("unit flags:\n%s", readFile(t, unitPath))
	}
	if !sd.took("systemctl --user daemon-reload") || !sd.took("systemctl --user enable stv2-syncthing.service") {
		t.Errorf("systemctl calls = %q", sd.calls)
	}
	if exists(filepath.Join(r.ConfigHome, "autostart", "stv2-syncthing.desktop")) {
		t.Error("the fallback desktop entry was left in place")
	}
	if on, err := m.Enabled(Syncthing); !on || err != nil {
		t.Errorf("Enabled = %v, %v", on, err)
	}
	if ok, err := m.StartService(Syncthing); !ok || err != nil || !sd.took("systemctl --user start stv2-syncthing.service") {
		t.Errorf("StartService = %v, %v; calls %q", ok, err, sd.calls)
	}
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("our own unit reported as foreign: %q", desc)
	}

	must(t, m.Set(Syncthing, false, "", nil))
	if exists(unitPath) || !sd.took("systemctl --user disable stv2-syncthing.service") {
		t.Errorf("unit not removed; calls %q", sd.calls)
	}
	if on, _ := m.Enabled(Syncthing); on {
		t.Error("still enabled after Set(false)")
	}
	if ok, _ := m.StartService(Syncthing); ok {
		t.Error("StartService after removal claimed a service")
	}
}

func TestXDGDistroUnit(t *testing.T) {
	r, root := xdgRoots(t)
	sd := newFakeSystemctl(r.ConfigHome)
	r.Run = sd.run
	m := &Manager{impl: newXDG(r)}

	distroBin := filepath.Join(root, "usr", "bin", "syncthing")
	must(t, os.MkdirAll(filepath.Dir(distroBin), 0o755))
	must(t, os.WriteFile(distroBin, []byte("binary"), 0o755))
	must(t, os.MkdirAll(r.UnitDirs[0], 0o755))
	must(t, os.WriteFile(filepath.Join(r.UnitDirs[0], "syncthing.service"),
		[]byte("[Service]\nExecStart="+systemdArg(distroBin)+" serve --no-browser --no-restart --logflags=0\n"), 0o644))

	// Enabled by someone else: foreign, and the toggle must not claim it.
	sd.enabled["syncthing.service"] = true
	if desc, ok := m.Existing(Syncthing); !ok || desc != "systemd user unit syncthing.service" {
		t.Errorf("Existing = %q, %v", desc, ok)
	}
	if on, _ := m.Enabled(Syncthing); on {
		t.Error("a unit enabled outside SyncThing V2 counts as ours")
	}
	delete(sd.enabled, "syncthing.service")

	// Enabled by us: the distro unit is used, and marked as ours.
	must(t, m.Set(Syncthing, true, distroBin, nil))
	if !sd.took("systemctl --user enable --now syncthing.service") {
		t.Errorf("systemctl calls = %q", sd.calls)
	}
	if exists(filepath.Join(r.ConfigHome, "systemd", "user", "stv2-syncthing.service")) {
		t.Error("our own unit was written for a distro binary")
	}
	if on, err := m.Enabled(Syncthing); !on || err != nil {
		t.Errorf("Enabled = %v, %v", on, err)
	}
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("the distro unit we enabled is reported as foreign: %q", desc)
	}
	if ok, err := m.StartService(Syncthing); !ok || err != nil || !sd.took("systemctl --user start syncthing.service") {
		t.Errorf("StartService = %v, %v", ok, err)
	}
	must(t, m.Set(Syncthing, false, "", nil))
	if !sd.took("systemctl --user disable syncthing.service") || sd.enabled["syncthing.service"] {
		t.Errorf("distro unit not disabled; calls %q", sd.calls)
	}
	if exists(filepath.Join(r.ConfigHome, "systemd", "user", "syncthing.service.d")) {
		t.Error("ownership drop-in left behind")
	}

	// A different binary gets our own unit even when a distro unit exists.
	other := filepath.Join(root, "managed", "syncthing")
	must(t, m.Set(Syncthing, true, other, nil))
	if !exists(filepath.Join(r.ConfigHome, "systemd", "user", "stv2-syncthing.service")) {
		t.Error("managed binary did not get stv2-syncthing.service")
	}

	// The system-wide syncthing@<user> unit is foreign too.
	must(t, m.Set(Syncthing, false, "", nil))
	sd.systemUnits["syncthing@alice.service"] = true
	if desc, ok := m.Existing(Syncthing); !ok || desc != "systemd system unit syncthing@alice.service" {
		t.Errorf("Existing = %q, %v", desc, ok)
	}
}

// ---------------------------------------------------------------------------
// The current OS through New and its real implementation

// roundTrip turns both targets on and off and checks Enabled after each step.
func roundTrip(t *testing.T, m *Manager, trayBin, syncthingBin string) {
	t.Helper()
	for _, tg := range []Target{Tray, Syncthing} {
		if on, err := m.Enabled(tg); err != nil || on {
			t.Fatalf("%v: Enabled before Set = %v, %v", tg, on, err)
		}
	}
	must(t, m.Set(Tray, true, trayBin, nil))
	must(t, m.Set(Syncthing, true, syncthingBin, nil))
	for _, tg := range []Target{Tray, Syncthing} {
		if on, err := m.Enabled(tg); err != nil || !on {
			t.Fatalf("%v: Enabled after Set(true) = %v, %v", tg, on, err)
		}
	}
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("our own entries reported as foreign: %q", desc)
	}
	must(t, m.Set(Tray, false, "", nil))
	if on, _ := m.Enabled(Tray); on {
		t.Error("tray still enabled after Set(false)")
	}
	if on, _ := m.Enabled(Syncthing); !on {
		t.Error("turning the tray off turned Syncthing off")
	}
	must(t, m.Set(Syncthing, false, "", nil))
	must(t, m.Set(Syncthing, false, "", nil)) // removing twice is fine
	if on, _ := m.Enabled(Syncthing); on {
		t.Error("syncthing still enabled after Set(false)")
	}
	if err := m.Set(Tray, true, "relative/stv2", nil); err == nil {
		t.Error("Set accepted a relative binary")
	}
	if err := m.Set(Target(9), true, trayBin, nil); err == nil {
		t.Error("Set accepted an unknown target")
	}
}

func TestNativeRoundTrip(t *testing.T) {
	root := t.TempDir()
	var r Roots
	switch runtime.GOOS {
	case "windows":
		r = windowsTestRoots(t, root)
	case "darwin":
		r = Roots{LaunchAgents: filepath.Join(root, "LaunchAgents"), LogDir: filepath.Join(root, "Logs"), UID: os.Getuid()}
	default:
		r = Roots{ConfigHome: filepath.Join(root, ".config"), UnitDirs: []string{filepath.Join(root, "units")}}
	}
	m := New(r)
	trayBin := filepath.Join(root, "App Dir", brand.ExeName())
	stBin := filepath.Join(root, "syncthing", "syncthing")
	roundTrip(t, m, trayBin, stBin)
	if runtime.GOOS == "windows" {
		windowsChecks(t, m, r, trayBin, stBin)
	}
}

// windowsTestRoots returns roots under a fresh HKCU test key, removed when
// the test ends. The registry is driven through reg.exe so this file builds
// on every OS.
func windowsTestRoots(t *testing.T, root string) Roots {
	base := fmt.Sprintf(`Software\SyncThingV2-autostart-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("reg", "delete", `HKCU\`+base, "/f").Run() })
	startup := filepath.Join(root, "Startup")
	must(t, os.MkdirAll(startup, 0o755))
	return Roots{
		RunKey:            base + `\Run`,
		ApprovedKey:       base + `\Approved`,
		ApprovedFolderKey: base + `\ApprovedFolder`,
		StartupDir:        startup,
	}
}

func reg(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("reg", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("reg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// regValue returns the data of a REG_SZ value, or "" when it is missing.
func regValue(t *testing.T, key, name string) string {
	t.Helper()
	out, err := exec.Command("reg", "query", `HKCU\`+key, "/v", name).CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, data, ok := strings.Cut(line, "REG_SZ"); ok {
			return strings.TrimSpace(data)
		}
	}
	return ""
}

func windowsChecks(t *testing.T, m *Manager, r Roots, trayBin, stBin string) {
	// Value names and command lines.
	must(t, m.Set(Tray, true, trayBin, nil))
	must(t, m.Set(Syncthing, true, stBin, nil))
	if got, want := regValue(t, r.RunKey, "SyncThingV2"), `"`+trayBin+`" --background`; got != want {
		t.Errorf("tray Run value = %s, want %s", got, want)
	}
	if got, want := regValue(t, r.RunKey, "SyncThingV2-Syncthing"), `"`+stBin+`" serve --no-console --no-browser`; got != want {
		t.Errorf("syncthing Run value = %s, want %s", got, want)
	}
	if ok, err := m.StartService(Syncthing); ok || err != nil {
		t.Errorf("StartService = %v, %v; Run values are not services", ok, err)
	}

	// Task Manager's "disabled" mark turns the entry off; Set(true) clears it.
	reg(t, "add", `HKCU\`+r.ApprovedKey, "/v", "SyncThingV2", "/t", "REG_BINARY", "/d", "030000000000000000000000", "/f")
	if on, _ := m.Enabled(Tray); on {
		t.Error("an entry disabled in Task Manager counts as enabled")
	}
	must(t, m.Set(Tray, true, trayBin, nil))
	if on, _ := m.Enabled(Tray); !on {
		t.Error("Set(true) did not clear the disabled mark")
	}

	// A foreign Run value that starts Syncthing.
	if _, ok := m.Existing(Syncthing); ok {
		t.Fatal("Existing before any foreign entry")
	}
	reg(t, "add", `HKCU\`+r.RunKey, "/v", "Other tray", "/t", "REG_SZ", "/d", `C:\Tools\SyncthingTray.exe`, "/f")
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("a non-Syncthing Run value was reported: %q", desc)
	}
	reg(t, "add", `HKCU\`+r.RunKey, "/v", "Syncthing", "/t", "REG_SZ", "/d", `"C:\Tools\syncthing.exe" serve --no-console`, "/f")
	if desc, ok := m.Existing(Syncthing); !ok || desc != "Run entry Syncthing" {
		t.Errorf("Existing = %q, %v", desc, ok)
	}
	reg(t, "add", `HKCU\`+r.ApprovedKey, "/v", "Syncthing", "/t", "REG_BINARY", "/d", "030000000000000000000000", "/f")
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("a disabled foreign Run value was reported: %q", desc)
	}

	// Startup-folder shortcuts: only one targeting syncthing.exe counts.
	must(t, os.WriteFile(filepath.Join(r.StartupDir, "Syncthing Tray.lnk"), buildLnk(lnkSpec{ansiPath: `C:\Tools\SyncthingTray.exe`}), 0o644))
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("a shortcut to another program was reported: %q", desc)
	}
	must(t, os.WriteFile(filepath.Join(r.StartupDir, "Syncthing.lnk"), buildLnk(lnkSpec{unicodePath: `C:\Users\u\Tools\syncthing.exe`, ansiPath: `C:\Users\u\Tools\syncthing.exe`, args: "serve --no-console --no-browser"}), 0o644))
	if desc, ok := m.Existing(Syncthing); !ok || desc != "Startup folder shortcut Syncthing.lnk" {
		t.Errorf("Existing = %q, %v", desc, ok)
	}
	reg(t, "add", `HKCU\`+r.ApprovedFolderKey, "/v", "Syncthing.lnk", "/t", "REG_BINARY", "/d", "030000000000000000000000", "/f")
	if desc, ok := m.Existing(Syncthing); ok {
		t.Errorf("a disabled shortcut was reported: %q", desc)
	}

	// Removing ours leaves foreign values alone.
	must(t, m.Set(Tray, false, "", nil))
	must(t, m.Set(Syncthing, false, "", nil))
	if regValue(t, r.RunKey, "SyncThingV2") != "" || regValue(t, r.RunKey, "SyncThingV2-Syncthing") != "" {
		t.Error("our Run values survived Set(false)")
	}
	if regValue(t, r.RunKey, "Syncthing") == "" {
		t.Error("Set(false) removed a foreign Run value")
	}
}

// ---------------------------------------------------------------------------

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTargetString(t *testing.T) {
	if Tray.String() != "tray" || Syncthing.String() != "syncthing" || Target(5).String() != "Target(5)" {
		t.Error("Target.String")
	}
}
