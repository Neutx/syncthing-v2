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
	"path"
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
// that wants syncing to go on checks Loaded before and starts Syncthing
// again unsupervised when the job was loaded.
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

// Loaded reports whether the service manager is running SyncThing V2's own
// job for t right now: on macOS, whether launchd has its LaunchAgent loaded
// (turning Syncthing's entry off then stops the Syncthing it runs). It is
// always false elsewhere.
func (m *Manager) Loaded(t Target) bool {
	if checkTarget(t) != nil {
		return false
	}
	l, ok := m.impl.(*launchd)
	return ok && l.loaded(t)
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

// AppBundle returns the name of the macOS app bundle (for example
// "Syncthing.app") whose Contents hold bin, or "". SyncThing V2 never keeps
// Syncthing inside a bundle, so a Syncthing found there belongs to that app,
// which starts it at login through its own login item.
func AppBundle(bin string) string {
	parts := strings.Split(filepath.ToSlash(bin), "/")
	for i := 0; i+1 < len(parts); i++ {
		if len(parts[i]) > len(".app") && strings.HasSuffix(strings.ToLower(parts[i]), ".app") && parts[i+1] == "Contents" {
			return parts[i]
		}
	}
	return ""
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

// enabled reports whether our plist for t is in place and launchd will load
// it at login. On macOS 13 and later the user can switch a LaunchAgent off
// under System Settings > General > Login Items ("Allow in the Background");
// the plist stays, but launchd records the label as disabled.
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
	if label != launchdLabel(t) {
		return false, nil
	}
	return !l.disabled(t), nil
}

// disabled reports whether launchd's override database marks our label for
// t as disabled (`launchctl print-disabled gui/<uid>`). Without launchctl, or
// when it fails, nothing is known to be disabled.
func (l *launchd) disabled(t Target) bool {
	if l.r.Run == nil {
		return false
	}
	out, err := l.r.Run(context.Background(), "launchctl", "print-disabled", "gui/"+strconv.Itoa(l.r.UID))
	if err != nil {
		return false
	}
	return labelDisabled(out, launchdLabel(t))
}

// labelDisabled parses `launchctl print-disabled` output, whose entries read
// `"label" => disabled` (macOS 13 and later) or `"label" => true` (earlier).
func labelDisabled(out []byte, label string) bool {
	quoted := strconv.Quote(label)
	for _, line := range strings.Split(string(out), "\n") {
		name, val, ok := strings.Cut(strings.TrimSpace(line), "=>")
		if !ok || strings.TrimSpace(name) != quoted {
			continue
		}
		switch strings.TrimSpace(val) {
		case "disabled", "true":
			return true
		}
		return false
	}
	return false
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
	if err := osutil.WriteFileAtomic(p, renderPlist(launchdLabel(t), append([]string{bin}, args...), t == Syncthing, logFile), 0o644); err != nil {
		return err
	}
	// A job switched off in System Settings > Login Items stays disabled
	// whatever the plist says; turning the toggle on clears that override.
	if l.disabled(t) {
		service := "gui/" + strconv.Itoa(l.r.UID) + "/" + launchdLabel(t)
		if _, err := l.r.Run(context.Background(), "launchctl", "enable", service); err != nil {
			return fmt.Errorf("autostart: it is turned off in System Settings > General > Login Items; turn it on there (launchctl enable: %w)", err)
		}
	}
	return nil
}

// existing reports a LaunchAgent other than ours whose program is syncthing
// (runsSyncthing), whatever the plist is called. A plist that cannot be read
// as XML (a binary plist) counts when its name contains "syncthing".
func (l *launchd) existing(Target) (string, bool) {
	entries, err := os.ReadDir(l.r.LaunchAgents)
	if err != nil {
		return "", false
	}
	own := []string{launchdLabel(Tray) + ".plist", launchdLabel(Syncthing) + ".plist"}
	for _, e := range entries {
		n := e.Name()
		lower := strings.ToLower(n)
		if !strings.HasSuffix(lower, ".plist") || slices.Contains(own, n) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(l.r.LaunchAgents, n))
		if err != nil {
			continue
		}
		vals, err := plistTopLevel(data)
		if err != nil || bytes.HasPrefix(data, []byte("bplist")) {
			if strings.Contains(lower, "syncthing") {
				return "LaunchAgent " + n, true
			}
			continue
		}
		if runsSyncthing(plistArgv(vals)) {
			return "LaunchAgent " + n, true
		}
	}
	return "", false
}

// plistArgv returns the command a launchd job runs: Program, when set, is the
// executable and ProgramArguments the argument vector.
func plistArgv(vals map[string]string) []string {
	var argv []string
	if a := vals["ProgramArguments"]; a != "" {
		argv = strings.Split(a, "\x00")
	}
	if p := vals["Program"]; p != "" {
		if len(argv) == 0 {
			return []string{p}
		}
		argv[0] = p
	}
	return argv
}

// runsSyncthing reports whether argv starts Syncthing: its program is named
// syncthing, or it is a shell or launcher (sh -c "exec syncthing ...", env
// syncthing ...) whose arguments name a syncthing program. Tools that only
// have "syncthing" in their name, such as syncthingtray, do not count.
func runsSyncthing(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	isSyncthing := func(p string) bool { return filepath.Base(strings.Trim(p, `"'`)) == "syncthing" }
	if isSyncthing(argv[0]) {
		return true
	}
	switch filepath.Base(argv[0]) {
	case "sh", "bash", "dash", "zsh", "env", "nice", "nohup":
		for _, a := range argv[1:] {
			for _, w := range strings.Fields(a) {
				if isSyncthing(w) {
					return true
				}
			}
		}
	}
	return false
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

// loaded reports whether launchd has our job for t (found by its label,
// which only SyncThing V2 uses) loaded in the gui/<uid> domain. `launchctl
// print` failing means it is not loaded.
func (l *launchd) loaded(t Target) bool {
	if l.r.Run == nil {
		return false
	}
	_, err := l.r.Run(context.Background(), "launchctl", "print", "gui/"+strconv.Itoa(l.r.UID)+"/"+launchdLabel(t))
	return err == nil
}

// unload boots our job for t out of the gui/<uid> domain when launchd has it
// loaded. A job that is not loaded needs no action; a job that is loaded but
// cannot be booted out is an error.
func (l *launchd) unload(t Target) error {
	if !l.loaded(t) {
		return nil
	}
	_, err := l.r.Run(context.Background(), "launchctl", "bootout", "gui/"+strconv.Itoa(l.r.UID)+"/"+launchdLabel(t))
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
	// Not --now, as for our own unit: the Syncthing this toggle is about is
	// normally already running (detected or started detached), and a second
	// instance under systemd would fail on the database lock until the unit
	// gives up. The unit takes over from the next login; stinstall.Start
	// starts it through StartService when Syncthing is down.
	_, err := x.systemctl("--user", "enable", distroUnit)
	return err
}

func (x *xdg) enableOwnUnit(bin string, args []string) error {
	if args == nil {
		args = []string{"serve", "--no-browser", "--no-restart"}
	}
	if err := os.MkdirAll(x.unitDir(), 0o755); err != nil {
		return err
	}
	if err := osutil.WriteFileAtomic(x.ownUnitPath(), renderUnit(append([]string{bin}, args...), !snapLauncher(bin)), 0o644); err != nil {
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

// existing reports a Syncthing autostart configured outside SyncThing V2: an
// enabled user unit or autostart entry whose program is syncthing (whatever
// it is called, for example Linuxbrew's homebrew.syncthing.service), or the
// system unit syncthing@<user>.service.
func (x *xdg) existing(Target) (string, bool) {
	if x.systemdAvailable() {
		if unit, ok := x.foreignUnit(); ok {
			return "systemd user unit " + unit, true
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
	own := []string{filepath.Base(x.desktopPath(Syncthing)), filepath.Base(x.desktopPath(Tray))}
	for _, e := range entries {
		n := e.Name()
		if slices.Contains(own, n) || !strings.HasSuffix(strings.ToLower(n), ".desktop") {
			continue
		}
		p := filepath.Join(x.autostartDir(), n)
		data, err := os.ReadFile(p)
		if err != nil || !runsSyncthing(desktopExecArgv(desktopEntry(data)["Exec"])) {
			continue
		}
		if on, _ := desktopActive(p); on {
			return "autostart entry " + n, true
		}
	}
	return "", false
}

// foreignUnit returns an enabled systemd user service, other than the ones
// SyncThing V2 enabled, whose ExecStart program is syncthing.
func (x *xdg) foreignUnit() (string, bool) {
	out, err := x.systemctl("--user", "list-unit-files", "--state=enabled", "--type=service", "--no-legend", "--plain")
	if err != nil {
		return "", false
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".service") || f[0] == ownUnit {
			continue
		}
		if f[0] == distroUnit && exists(x.dropInPath()) {
			continue // enabled by SyncThing V2
		}
		units = append(units, f[0])
	}
	if len(units) == 0 {
		return "", false
	}
	// One call for all units. A unit that cannot be shown makes systemctl
	// fail, but the others are still printed.
	cat, _ := x.systemctl(append([]string{"--user", "cat", "--"}, units...)...)
	progs := unitPrograms(cat)
	for _, u := range units {
		for _, p := range progs[u] {
			if runsSyncthing([]string{p}) {
				return u, true
			}
		}
	}
	return "", false
}

// unitPrograms reads `systemctl cat` output, where each file is headed by a
// "# /path/name.service" or "# /path/name.service.d/x.conf" line, and returns
// the ExecStart programs of each unit. An empty ExecStart= in a drop-in
// resets the list, as it does for systemd.
func unitPrograms(data []byte) map[string][]string {
	out := map[string][]string{}
	unit := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if p, ok := strings.CutPrefix(line, "# /"); ok && (strings.HasSuffix(p, ".service") || strings.HasSuffix(p, ".conf")) {
			p = "/" + p
			if dir := path.Dir(p); strings.HasSuffix(dir, ".d") {
				unit = path.Base(strings.TrimSuffix(dir, ".d"))
			} else {
				unit = path.Base(p)
			}
			continue
		}
		if unit == "" || !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		if prog := unitExec([]byte(line)); prog != "" {
			out[unit] = append(out[unit], prog)
		} else if strings.TrimSpace(strings.TrimPrefix(line, "ExecStart=")) == "" {
			out[unit] = nil
		}
	}
	return out
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

// desktopExecArgv splits a desktop entry's Exec value (as desktopEntry
// returns it) into arguments, undoing what desktopExecArg applies: the
// string-value escapes, double quotes with backslash escapes, and the
// doubled "%". Field codes such as %U are kept as they are.
func desktopExecArgv(exec string) []string {
	s := strings.NewReplacer(`\\`, `\`, `\s`, " ", `\n`, "\n", `\t`, "\t", `\r`, "\r").Replace(exec)
	var argv []string
	var word strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\\' && i+1 < len(s):
			i++
			word.WriteByte(s[i])
		case c == '"':
			quoted, inWord = !quoted, true
		case !quoted && (c == ' ' || c == '\t' || c == '\n'):
			if inWord {
				argv = append(argv, word.String())
				word.Reset()
				inWord = false
			}
		case c == '%' && i+1 < len(s) && s[i+1] == '%':
			i++
			word.WriteByte('%')
			inWord = true
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		argv = append(argv, word.String())
	}
	return argv
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
// snapLauncher reports whether bin starts a snap: it lies under /snap/ or
// resolves to the snap command (/snap/bin/syncthing is a link to
// /usr/bin/snap). snap run execs the setuid-root snap-confine, which cannot
// work under NoNewPrivileges.
func snapLauncher(bin string) bool {
	if strings.HasPrefix(filepath.ToSlash(bin), "/snap/") {
		return true
	}
	real, err := filepath.EvalSymlinks(bin)
	return err == nil && (filepath.Base(real) == "snap" || strings.HasPrefix(filepath.ToSlash(real), "/snap/"))
}

// renderUnit writes our Syncthing user unit. hardened adds
// MemoryDenyWriteExecute and NoNewPrivileges, which a snap's launcher cannot
// run under (see snapLauncher).
func renderUnit(argv []string, hardened bool) []byte {
	exec := make([]string, len(argv))
	for i, a := range argv {
		exec[i] = systemdArg(a)
	}
	hardening := "SystemCallArchitectures=native\n"
	if hardened {
		hardening += "MemoryDenyWriteExecute=true\nNoNewPrivileges=true\n"
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
` + hardening + `
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
