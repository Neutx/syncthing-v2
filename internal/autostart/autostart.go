// Package autostart manages the login entries that start the SyncThing V2
// tray and Syncthing (spec §3.5, §5.3):
//
//	Windows: HKCU ...\CurrentVersion\Run values
//	macOS:   LaunchAgent plists in ~/Library/LaunchAgents
//	Linux:   XDG autostart .desktop files and systemd user units
//
// SyncThing V2 creates or removes only entries it owns. Existing reports
// Syncthing autostart entries configured outside SyncThing V2 (a Startup
// folder shortcut, another Run value, another LaunchAgent, an enabled distro
// unit), which the UI then shows read-only.
//
// Every location and helper command is a field of Roots, so tests work on
// temporary directories, a test-only registry key and fake commands. The
// launchd and systemd/XDG implementations are plain file and command logic;
// they live in this file so that their tests run on every OS, and the per-OS
// files only pick the implementation and its real roots.
package autostart

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// Target selects which program an entry starts.
type Target int

const (
	// Tray is the SyncThing V2 tray (started with --background).
	Tray Target = iota
	// Syncthing is the Syncthing daemon.
	Syncthing
)

func (t Target) String() string {
	switch t {
	case Tray:
		return "tray"
	case Syncthing:
		return "syncthing"
	}
	return "Target(" + strconv.Itoa(int(t)) + ")"
}

// ExternalText is how the UI describes a Syncthing autostart configured
// outside SyncThing V2.
const ExternalText = "Syncthing starts at login (configured outside SyncThing V2)"

// Roots are the locations and helper commands a Manager works with. Fields
// for other operating systems are ignored.
type Roots struct {
	// Windows. Keys are relative to HKEY_CURRENT_USER.
	RunKey            string // Run values
	ApprovedKey       string // Explorer\StartupApproved\Run ("" = not checked)
	ApprovedFolderKey string // Explorer\StartupApproved\StartupFolder ("" = not checked)
	StartupDir        string // the user's Startup folder

	// macOS.
	LaunchAgents string // ~/Library/LaunchAgents
	LogDir       string // ~/Library/Logs/SyncThingV2 (Syncthing's log file)
	UID          int    // for launchctl's gui/<uid> domain

	// Linux.
	ConfigHome string   // $XDG_CONFIG_HOME, default ~/.config
	UnitDirs   []string // directories that may hold a distro syncthing.service
	User       string   // user name, for the system unit syncthing@<user>.service

	// Run executes a helper command (launchctl, systemctl) and returns its
	// standard output. nil means no helper commands are available.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Manager reads and writes autostart entries under one set of Roots.
type Manager struct{ impl impl }

type impl interface {
	enabled(t Target) (bool, error)
	set(t Target, on bool, bin string, args []string) error
	existing(t Target) (string, bool)
	startService(t Target) (bool, error)
}

// Enabled reports whether SyncThing V2's own entry for t is present and
// active.
func (m *Manager) Enabled(t Target) (bool, error) {
	if err := checkTarget(t); err != nil {
		return false, err
	}
	return m.impl.enabled(t)
}

// Set creates (on) or removes (off) SyncThing V2's own entry for t. bin must
// be an absolute path when on is true; args nil selects the default flags:
// --background for the tray, the per-OS serve flags for Syncthing. Removing
// an entry that does not exist is not an error. Foreign entries are never
// touched.
//
// On macOS, turning Syncthing's entry off also unloads its launchd job
// (launchctl bootout), because launchd would otherwise keep restarting
// Syncthing until logout. That stops a Syncthing launchd started; a caller
// that wants syncing to go on starts Syncthing again unsupervised.
func (m *Manager) Set(t Target, on bool, bin string, args []string) error {
	if err := checkTarget(t); err != nil {
		return err
	}
	if on {
		if !filepath.IsAbs(bin) {
			return fmt.Errorf("autostart: %q is not an absolute path", bin)
		}
		bin = filepath.Clean(bin)
	}
	return m.impl.set(t, on, bin, args)
}

// Existing reports a Syncthing autostart entry configured outside SyncThing
// V2, with a short description of it. SyncThing V2's own entries are never
// reported. The tray has no foreign entries, so Existing(Tray) is always
// false.
func (m *Manager) Existing(t Target) (desc string, ok bool) {
	if t != Syncthing {
		return "", false
	}
	return m.impl.existing(t)
}

// StartService starts t through the service manager when SyncThing V2's own
// entry for it is a launchd job or systemd unit. registered is false when
// there is no such entry (Windows Run values and XDG autostart files are not
// services); the caller then starts the program itself.
func (m *Manager) StartService(t Target) (registered bool, err error) {
	if err := checkTarget(t); err != nil {
		return false, err
	}
	return m.impl.startService(t)
}

func checkTarget(t Target) error {
	if t != Tray && t != Syncthing {
		return fmt.Errorf("autostart: unknown target %d", int(t))
	}
	return nil
}

var (
	defaultOnce sync.Once
	defaultMgr  *Manager
	defaultErr  error
)

// Default returns the Manager for the current user's real locations.
func Default() (*Manager, error) {
	defaultOnce.Do(func() {
		r, err := DefaultRoots()
		if err != nil {
			defaultErr = err
			return
		}
		defaultMgr = New(r)
	})
	return defaultMgr, defaultErr
}

// Enabled calls Default().Enabled.
func Enabled(t Target) (bool, error) {
	m, err := Default()
	if err != nil {
		return false, err
	}
	return m.Enabled(t)
}

// Set calls Default().Set.
func Set(t Target, on bool, bin string, args []string) error {
	m, err := Default()
	if err != nil {
		return err
	}
	return m.Set(t, on, bin, args)
}

// Existing calls Default().Existing.
func Existing(t Target) (desc string, ok bool) {
	m, err := Default()
	if err != nil {
		return "", false
	}
	return m.Existing(t)
}

// StartService calls Default().StartService.
func StartService(t Target) (bool, error) {
	m, err := Default()
	if err != nil {
		return false, err
	}
	return m.StartService(t)
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

// ---------------------------------------------------------------------------
// Command lines

// ReferencesSyncthing reports whether a Windows command line starts
// Syncthing: its program is syncthing(.exe), or it names syncthing.exe
// anywhere (for example through cmd /c start).
func ReferencesSyncthing(cmdline string) bool {
	lower := strings.ToLower(cmdline)
	if strings.Contains(lower, "syncthing.exe") {
		return true
	}
	exe := commandProgram(lower)
	return exe == "syncthing" || strings.HasSuffix(exe, `\syncthing`) || strings.HasSuffix(exe, "/syncthing")
}

// commandProgram returns the program part of a Windows command line.
func commandProgram(cmdline string) string {
	s := strings.TrimSpace(cmdline)
	if strings.HasPrefix(s, `"`) {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : end+1]
		}
		return s[1:]
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// Windows shell links (.lnk, MS-SHLLINK)

// ShellLink is the part of a .lnk file autostart needs.
type ShellLink struct {
	Target string // local path from LinkInfo, or the relative path
	Args   string
}

const (
	lnkHasIDList       = 1 << 0
	lnkHasLinkInfo     = 1 << 1
	lnkHasName         = 1 << 2
	lnkHasRelativePath = 1 << 3
	lnkHasWorkingDir   = 1 << 4
	lnkHasArguments    = 1 << 5
	lnkHasIconLocation = 1 << 6
	lnkIsUnicode       = 1 << 7

	lnkHeaderSize     = 0x4C
	linkInfoLocalPath = 1 << 0
)

// ParseShellLink reads the target path and arguments of a Windows shell
// link. It understands the LinkInfo local path (ANSI or Unicode) and falls
// back to the relative path from StringData.
func ParseShellLink(data []byte) (ShellLink, error) {
	bad := func(what string) (ShellLink, error) {
		return ShellLink{}, fmt.Errorf("not a valid shell link: %s", what)
	}
	if len(data) < lnkHeaderSize || binary.LittleEndian.Uint32(data) != lnkHeaderSize {
		return bad("header")
	}
	flags := binary.LittleEndian.Uint32(data[0x14:])
	off := lnkHeaderSize
	if flags&lnkHasIDList != 0 {
		if off+2 > len(data) {
			return bad("ID list")
		}
		off += 2 + int(binary.LittleEndian.Uint16(data[off:]))
	}
	var link ShellLink
	if flags&lnkHasLinkInfo != 0 {
		if off+4 > len(data) {
			return bad("link info")
		}
		size := int(binary.LittleEndian.Uint32(data[off:]))
		if size < 0x1C || off+size > len(data) {
			return bad("link info size")
		}
		link.Target = linkInfoPath(data[off : off+size])
		off += size
	}
	unicode := flags&lnkIsUnicode != 0
	for _, bit := range []uint32{lnkHasName, lnkHasRelativePath, lnkHasWorkingDir, lnkHasArguments, lnkHasIconLocation} {
		if flags&bit == 0 {
			continue
		}
		s, n, ok := lnkString(data[off:], unicode)
		if !ok {
			return bad("string data")
		}
		off += n
		switch bit {
		case lnkHasRelativePath:
			if link.Target == "" {
				link.Target = s
			}
		case lnkHasArguments:
			link.Args = s
		}
	}
	if link.Target == "" {
		return bad("no target path")
	}
	return link, nil
}

// linkInfoPath returns LocalBasePath + CommonPathSuffix from a LinkInfo
// structure, preferring the Unicode fields.
func linkInfoPath(li []byte) string {
	u32 := func(o int) int {
		if o+4 > len(li) {
			return 0
		}
		return int(binary.LittleEndian.Uint32(li[o:]))
	}
	headerSize, flags := u32(4), u32(8)
	if flags&linkInfoLocalPath == 0 {
		return ""
	}
	if headerSize >= 0x24 {
		base := utf16z(li, u32(0x1C))
		suffix := utf16z(li, u32(0x20))
		if base != "" {
			return base + suffix
		}
	}
	return ansiz(li, u32(0x10)) + ansiz(li, u32(0x18))
}

// ansiz reads a NUL-terminated single-byte string at off. Bytes above 0x7F
// are taken as Latin-1, which keeps ASCII file names such as syncthing.exe
// intact whatever the code page.
func ansiz(b []byte, off int) string {
	if off <= 0 || off >= len(b) {
		return ""
	}
	end := bytes.IndexByte(b[off:], 0)
	if end < 0 {
		return ""
	}
	r := make([]rune, 0, end)
	for _, c := range b[off : off+end] {
		r = append(r, rune(c))
	}
	return string(r)
}

// utf16z reads a NUL-terminated UTF-16LE string at off.
func utf16z(b []byte, off int) string {
	if off <= 0 || off >= len(b) {
		return ""
	}
	var u []uint16
	for i := off; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			return string(utf16.Decode(u))
		}
		u = append(u, c)
	}
	return ""
}

// lnkString reads one StringData entry: a character count followed by the
// characters. It returns the string and the bytes consumed.
func lnkString(b []byte, unicode bool) (string, int, bool) {
	if len(b) < 2 {
		return "", 0, false
	}
	n := int(binary.LittleEndian.Uint16(b))
	if !unicode {
		if 2+n > len(b) {
			return "", 0, false
		}
		r := make([]rune, n)
		for i, c := range b[2 : 2+n] {
			r[i] = rune(c)
		}
		return string(r), 2 + n, true
	}
	if 2+2*n > len(b) {
		return "", 0, false
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2+2*i:])
	}
	return string(utf16.Decode(u)), 2 + 2*n, true
}

// ---------------------------------------------------------------------------
// launchd (macOS)

// launchdLabel returns the LaunchAgent label for t.
func launchdLabel(t Target) string {
	if t == Syncthing {
		return brand.BundleID + ".syncthing"
	}
	return brand.BundleID
}

type launchd struct{ r Roots }

func newLaunchd(r Roots) *launchd { return &launchd{r: r} }

func (l *launchd) plistPath(t Target) string {
	return filepath.Join(l.r.LaunchAgents, launchdLabel(t)+".plist")
}

func (l *launchd) enabled(t Target) (bool, error) {
	data, err := os.ReadFile(l.plistPath(t))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	label, err := plistLabel(data)
	if err != nil {
		return false, fmt.Errorf("autostart: %s: %w", l.plistPath(t), err)
	}
	return label == launchdLabel(t), nil
}

func (l *launchd) set(t Target, on bool, bin string, args []string) error {
	p := l.plistPath(t)
	if !on {
		if t != Syncthing {
			return removeIfExists(p)
		}
		// The Syncthing job is KeepAlive: while it stays loaded, launchd
		// restarts Syncthing whenever it exits, until logout. Unload it
		// (which stops the Syncthing it runs), then remove the plist. The
		// plist goes even when unloading fails, so the next login does not
		// start Syncthing.
		unloadErr := l.unload(t)
		if err := removeIfExists(p); err != nil {
			return errors.Join(err, unloadErr)
		}
		if unloadErr != nil {
			return fmt.Errorf("autostart: Syncthing no longer starts at login, but launchd keeps it running until logout: %w", unloadErr)
		}
		return nil
	}
	if args == nil {
		args = []string{"--background"}
		if t == Syncthing {
			args = []string{"serve", "--no-browser", "--no-restart"}
		}
	}
	if err := os.MkdirAll(l.r.LaunchAgents, 0o755); err != nil {
		return err
	}
	logFile := ""
	if t == Syncthing {
		if err := os.MkdirAll(l.r.LogDir, 0o700); err != nil {
			return err
		}
		logFile = filepath.Join(l.r.LogDir, "syncthing.log")
	}
	return osutil.WriteFileAtomic(p, renderPlist(launchdLabel(t), append([]string{bin}, args...), t == Syncthing, logFile), 0o644)
}

// existing lists ~/Library/LaunchAgents/*syncthing*.plist other than ours.
func (l *launchd) existing(Target) (string, bool) {
	entries, err := os.ReadDir(l.r.LaunchAgents)
	if err != nil {
		return "", false
	}
	own := []string{launchdLabel(Tray) + ".plist", launchdLabel(Syncthing) + ".plist"}
	for _, e := range entries {
		n := e.Name()
		lower := strings.ToLower(n)
		if !strings.HasSuffix(lower, ".plist") || !strings.Contains(lower, "syncthing") || slices.Contains(own, n) {
			continue
		}
		return "LaunchAgent " + n, true
	}
	return "", false
}

// startService loads (bootstrap) or starts (kickstart) our Syncthing job.
func (l *launchd) startService(t Target) (bool, error) {
	if t != Syncthing || l.r.Run == nil {
		return false, nil
	}
	if on, err := l.enabled(t); err != nil || !on {
		return false, err
	}
	ctx := context.Background()
	domain := "gui/" + strconv.Itoa(l.r.UID)
	if _, err := l.r.Run(ctx, "launchctl", "bootstrap", domain, l.plistPath(t)); err == nil {
		return true, nil
	}
	// Already loaded: start it if it is not running.
	_, err := l.r.Run(ctx, "launchctl", "kickstart", domain+"/"+launchdLabel(t))
	return true, err
}

// unload boots our job for t (found by its label, which only SyncThing V2
// uses) out of the gui/<uid> domain when launchd has it loaded. `launchctl
// print` failing means it is not loaded, which needs no action; a job that
// is loaded but cannot be booted out is an error.
func (l *launchd) unload(t Target) error {
	if l.r.Run == nil {
		return nil
	}
	ctx := context.Background()
	service := "gui/" + strconv.Itoa(l.r.UID) + "/" + launchdLabel(t)
	if _, err := l.r.Run(ctx, "launchctl", "print", service); err != nil {
		return nil
	}
	_, err := l.r.Run(ctx, "launchctl", "bootout", service)
	return err
}

// renderPlist writes a LaunchAgent property list. The Syncthing job is kept
// alive by launchd and logs to logFile; the tray job runs once per login in
// the Aqua session.
func renderPlist(label string, argv []string, daemon bool, logFile string) []byte {
	var b bytes.Buffer
	str := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return "<string>" + e.String() + "</string>"
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	` + str(label) + `
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range argv {
		b.WriteString("\t\t" + str(a) + "\n")
	}
	b.WriteString("\t</array>\n\t<key>RunAtLoad</key>\n\t<true/>\n")
	if daemon {
		b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
		b.WriteString("\t<key>ProcessType</key>\n\t" + str("Background") + "\n")
		b.WriteString("\t<key>LowPriorityIO</key>\n\t<true/>\n")
		b.WriteString("\t<key>StandardOutPath</key>\n\t" + str(logFile) + "\n")
		b.WriteString("\t<key>StandardErrorPath</key>\n\t" + str(logFile) + "\n")
	} else {
		b.WriteString("\t<key>ProcessType</key>\n\t" + str("Interactive") + "\n")
		b.WriteString("\t<key>LimitLoadToSessionType</key>\n\t" + str("Aqua") + "\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// plistLabel returns the Label of a property list.
func plistLabel(data []byte) (string, error) {
	vals, err := plistTopLevel(data)
	if err != nil {
		return "", err
	}
	return vals["Label"], nil
}

// plistTopLevel returns the string values of the top-level dict, and the
// ProgramArguments array joined with "\x00" under "ProgramArguments".
func plistTopLevel(data []byte) (map[string]string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	out := map[string]string{}
	depth := 0
	key, inKey, inArray := "", false, false
	var args []string
	var text strings.Builder
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			text.Reset()
			switch {
			case t.Name.Local == "key" && depth == 3:
				inKey = true
			case t.Name.Local == "array" && depth == 3 && key == "ProgramArguments":
				inArray = true
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch {
			case t.Name.Local == "key" && inKey:
				key, inKey = text.String(), false
			case t.Name.Local == "string" && depth == 3 && key != "":
				out[key] = text.String()
				key = ""
			case t.Name.Local == "string" && depth == 4 && inArray:
				args = append(args, text.String())
			case t.Name.Local == "array" && inArray:
				out["ProgramArguments"] = strings.Join(args, "\x00")
				inArray, key = false, ""
			case depth == 3 && t.Name.Local != "key":
				key = ""
			}
			depth--
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// systemd and XDG autostart (Linux)

const (
	ownUnit     = "stv2-syncthing.service"
	distroUnit  = "syncthing.service"
	ownershipDI = "stv2-owned.conf" // drop-in marking a distro unit SyncThing V2 enabled
)

type xdg struct{ r Roots }

func newXDG(r Roots) *xdg { return &xdg{r: r} }

func (x *xdg) autostartDir() string { return filepath.Join(x.r.ConfigHome, "autostart") }
func (x *xdg) unitDir() string      { return filepath.Join(x.r.ConfigHome, "systemd", "user") }

func (x *xdg) desktopPath(t Target) string {
	name := brand.BinaryName + ".desktop"
	if t == Syncthing {
		name = brand.BinaryName + "-syncthing.desktop"
	}
	return filepath.Join(x.autostartDir(), name)
}

func (x *xdg) ownUnitPath() string { return filepath.Join(x.unitDir(), ownUnit) }
func (x *xdg) wantsLink() string {
	return filepath.Join(x.unitDir(), "default.target.wants", ownUnit)
}
func (x *xdg) dropInPath() string {
	return filepath.Join(x.unitDir(), distroUnit+".d", ownershipDI)
}

func (x *xdg) systemctl(args ...string) ([]byte, error) {
	if x.r.Run == nil {
		return nil, errors.New("systemctl is not available")
	}
	return x.r.Run(context.Background(), "systemctl", args...)
}

// systemdAvailable reports whether a systemd user manager answers.
func (x *xdg) systemdAvailable() bool {
	_, err := x.systemctl("--user", "show-environment")
	return err == nil
}

func (x *xdg) enabled(t Target) (bool, error) {
	if t == Tray {
		return desktopActive(x.desktopPath(Tray))
	}
	if exists(x.ownUnitPath()) && exists(x.wantsLink()) {
		return true, nil
	}
	if exists(x.dropInPath()) {
		out, err := x.systemctl("--user", "is-enabled", distroUnit)
		if err == nil && strings.TrimSpace(string(out)) == "enabled" {
			return true, nil
		}
	}
	return desktopActive(x.desktopPath(Syncthing))
}

func (x *xdg) set(t Target, on bool, bin string, args []string) error {
	if t == Tray {
		if !on {
			return removeIfExists(x.desktopPath(Tray))
		}
		if args == nil {
			args = []string{"--background"}
		}
		return x.writeDesktop(x.desktopPath(Tray), brand.DisplayName, "Tray status for Syncthing", append([]string{bin}, args...))
	}
	// Whatever mechanism we used before goes first, so a switch (for
	// example from the XDG fallback to a unit) never leaves two entries that
	// would each start an instance.
	if err := x.disableSyncthing(); err != nil || !on {
		return err
	}
	if !x.systemdAvailable() {
		if args == nil {
			// Nothing supervises it, so Syncthing's own monitor restarts it.
			args = []string{"serve", "--no-browser"}
		}
		return x.writeDesktop(x.desktopPath(Syncthing), "Syncthing", "Starts Syncthing for SyncThing V2", append([]string{bin}, args...))
	}
	if args == nil && x.isDistroBinary(bin) {
		return x.enableDistroUnit()
	}
	return x.enableOwnUnit(bin, args)
}

func (x *xdg) enableDistroUnit() error {
	if err := os.MkdirAll(filepath.Dir(x.dropInPath()), 0o755); err != nil {
		return err
	}
	dropIn := "# SyncThing V2 enabled syncthing.service. Turning Syncthing autostart off in\n" +
		"# SyncThing V2 disables the unit again and removes this file.\n" +
		"[Unit]\nX-SyncThingV2-Owned=yes\n"
	if err := osutil.WriteFileAtomic(x.dropInPath(), []byte(dropIn), 0o644); err != nil {
		return err
	}
	if _, err := x.systemctl("--user", "daemon-reload"); err != nil {
		return err
	}
	_, err := x.systemctl("--user", "enable", "--now", distroUnit)
	return err
}

func (x *xdg) enableOwnUnit(bin string, args []string) error {
	if args == nil {
		args = []string{"serve", "--no-browser", "--no-restart"}
	}
	if err := os.MkdirAll(x.unitDir(), 0o755); err != nil {
		return err
	}
	if err := osutil.WriteFileAtomic(x.ownUnitPath(), renderUnit(append([]string{bin}, args...)), 0o644); err != nil {
		return err
	}
	if _, err := x.systemctl("--user", "daemon-reload"); err != nil {
		return err
	}
	// Not --now: Syncthing is normally already running (started detached);
	// the unit takes over from the next login.
	_, err := x.systemctl("--user", "enable", ownUnit)
	return err
}

func (x *xdg) disableSyncthing() error {
	var errs []error
	systemd := x.systemdAvailable()
	reload := false
	if exists(x.dropInPath()) {
		if systemd {
			if _, err := x.systemctl("--user", "disable", distroUnit); err != nil {
				errs = append(errs, err)
			}
		}
		errs = append(errs, removeIfExists(x.dropInPath()))
		_ = os.Remove(filepath.Dir(x.dropInPath())) // only succeeds when empty
		reload = true
	}
	if exists(x.ownUnitPath()) || exists(x.wantsLink()) {
		if systemd {
			if _, err := x.systemctl("--user", "disable", ownUnit); err != nil {
				errs = append(errs, err)
			}
		}
		errs = append(errs, removeIfExists(x.wantsLink()), removeIfExists(x.ownUnitPath()))
		reload = true
	}
	if reload && systemd {
		if _, err := x.systemctl("--user", "daemon-reload"); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, removeIfExists(x.desktopPath(Syncthing)))
	return errors.Join(errs...)
}

// isDistroBinary reports whether a distro user unit syncthing.service runs
// bin, in which case that unit is enabled instead of writing our own.
func (x *xdg) isDistroBinary(bin string) bool {
	for _, dir := range x.r.UnitDirs {
		data, err := os.ReadFile(filepath.Join(dir, distroUnit))
		if err != nil {
			continue
		}
		if exe := unitExec(data); exe != "" && sameFile(exe, bin) {
			return true
		}
	}
	return false
}

func (x *xdg) existing(Target) (string, bool) {
	if x.systemdAvailable() {
		out, err := x.systemctl("--user", "list-unit-files", "--state=enabled", "--no-legend", "--plain", "syncthing*.service")
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				f := strings.Fields(line)
				if len(f) == 0 || !strings.HasPrefix(f[0], "syncthing") {
					continue
				}
				if f[0] == distroUnit && exists(x.dropInPath()) {
					continue // enabled by SyncThing V2
				}
				return "systemd user unit " + f[0], true
			}
		}
	}
	if x.r.User != "" && x.r.Run != nil {
		unit := "syncthing@" + x.r.User + ".service"
		if out, err := x.systemctl("is-enabled", unit); err == nil && strings.TrimSpace(string(out)) == "enabled" {
			return "systemd system unit " + unit, true
		}
	}
	entries, err := os.ReadDir(x.autostartDir())
	if err != nil {
		return "", false
	}
	own := filepath.Base(x.desktopPath(Syncthing))
	for _, e := range entries {
		n := e.Name()
		lower := strings.ToLower(n)
		if n == own || !strings.HasSuffix(lower, ".desktop") || !strings.Contains(lower, "syncthing") {
			continue
		}
		if on, _ := desktopActive(filepath.Join(x.autostartDir(), n)); on {
			return "autostart entry " + n, true
		}
	}
	return "", false
}

func (x *xdg) startService(t Target) (bool, error) {
	if t != Syncthing || !x.systemdAvailable() {
		return false, nil
	}
	unit := ""
	switch {
	case exists(x.ownUnitPath()) && exists(x.wantsLink()):
		unit = ownUnit
	case exists(x.dropInPath()):
		unit = distroUnit
	default:
		return false, nil
	}
	_, err := x.systemctl("--user", "start", unit)
	return true, err
}

func (x *xdg) writeDesktop(p, name, comment string, argv []string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(p, renderDesktop(name, comment, argv), 0o644)
}

// renderDesktop writes an XDG autostart entry.
func renderDesktop(name, comment string, argv []string) []byte {
	exec := make([]string, len(argv))
	for i, a := range argv {
		exec[i] = desktopExecArg(a)
	}
	return []byte("[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=" + desktopValue(name) + "\n" +
		"Comment=" + desktopValue(comment) + "\n" +
		"Exec=" + strings.Join(exec, " ") + "\n" +
		"Icon=" + brand.BinaryName + "\n" +
		"Terminal=false\n" +
		"NoDisplay=true\n" +
		"X-GNOME-Autostart-enabled=true\n")
}

// desktopValue escapes a desktop-entry string value.
func desktopValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return r.Replace(s)
}

// desktopExecArg quotes one Exec argument per the Desktop Entry
// specification: arguments with reserved characters are double-quoted with
// `"`, "`", "$" and "\" backslash-escaped, "%" is doubled, and the result is
// escaped once more as a string value.
func desktopExecArg(a string) string {
	const reserved = " \t\n\"'\\><~|&;$*?#()`"
	q := strings.ReplaceAll(a, "%", "%%")
	if a == "" || strings.ContainsAny(a, reserved) {
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range q {
			if strings.ContainsRune("\"`$\\", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
		q = b.String()
	}
	return strings.ReplaceAll(q, `\`, `\\`)
}

// desktopActive reports whether an autostart .desktop file exists and is not
// hidden or disabled.
func desktopActive(p string) (bool, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	kv := desktopEntry(data)
	if strings.EqualFold(kv["Hidden"], "true") || strings.EqualFold(kv["X-GNOME-Autostart-enabled"], "false") {
		return false, nil
	}
	return true, nil
}

// desktopEntry returns the keys of the [Desktop Entry] group.
func desktopEntry(data []byte) map[string]string {
	kv := map[string]string{}
	in := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = line == "[Desktop Entry]"
			continue
		}
		if !in || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

// renderUnit writes stv2-syncthing.service with upstream's user-unit
// semantics (etc/linux-systemd/user/syncthing.service).
func renderUnit(argv []string) []byte {
	exec := make([]string, len(argv))
	for i, a := range argv {
		exec[i] = systemdArg(a)
	}
	return []byte(`[Unit]
Description=Syncthing (started by ` + brand.DisplayName + `)
Documentation=man:syncthing(1)
StartLimitIntervalSec=60
StartLimitBurst=4

[Service]
Environment="STLOGFORMATTIMESTAMP="
Environment="STLOGFORMATLEVELSTRING=false"
Environment="STLOGFORMATLEVELSYSLOG=true"
ExecStart=` + strings.Join(exec, " ") + `
Restart=on-failure
RestartSec=1
SuccessExitStatus=3 4
RestartForceExitStatus=3 4

# Hardening
SystemCallArchitectures=native
MemoryDenyWriteExecute=true
NoNewPrivileges=true

[Install]
WantedBy=default.target
`)
}

// systemdArg quotes one ExecStart argument: specifiers (%) and variable
// expansion ($) are escaped, and arguments with spaces, quotes or
// backslashes are double-quoted.
func systemdArg(a string) string {
	q := strings.NewReplacer("%", "%%", "$", "$$").Replace(a)
	if a == "" || strings.ContainsAny(a, " \t\"'\\;") {
		q = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(q) + `"`
	}
	return q
}

// unitExec returns the program of a unit's first ExecStart= line, with
// systemd's quoting, backslash escapes, %% and $$ undone.
func unitExec(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ExecStart=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(strings.TrimLeft(v, "-@:+!"))
		var word strings.Builder
		quoted := strings.HasPrefix(v, `"`)
		if quoted {
			v = v[1:]
		}
		closed := !quoted
		for i := 0; i < len(v); i++ {
			c := v[i]
			switch {
			case c == '\\' && i+1 < len(v):
				i++
				word.WriteByte(v[i])
				continue
			case quoted && c == '"':
				closed = true
			case !quoted && (c == ' ' || c == '\t'):
			default:
				word.WriteByte(c)
				continue
			}
			break
		}
		if !closed {
			return ""
		}
		return strings.NewReplacer("%%", "%", "$$", "$").Replace(word.String())
	}
	return ""
}

// sameFile reports whether two paths name the same file, following
// symlinks (for example /bin -> /usr/bin on merged-/usr systems).
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
