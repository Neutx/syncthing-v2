package status

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// Diagnostics renders a plain-text status report for "Copy diagnostics"
// (F17), safe to paste into a public issue:
//
//   - no API key (registered secrets and key=value / header patterns are removed)
//   - device IDs cut to their first 7 characters
//   - no device names, folder labels, folder IDs, file names or paths
//   - no IP addresses except loopback and tailnet (100.64.0.0/10) ones
//
// extra adds "key: value" lines (sorted by key); values are redacted the same way.
func Diagnostics(s model.Snapshot, extra map[string]string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("%s %s diagnostics (%s/%s)", brand.DisplayName, brand.Version, runtime.GOOS, runtime.GOARCH)
	if !s.At.IsZero() {
		line("Generated: %s", s.At.UTC().Format(time.RFC3339))
	}
	line("Status: %s", Label(s.State))
	if s.Detail != "" {
		line("Detail: %s", RedactText(s.Detail))
	}

	connected := 0
	for _, p := range s.Peers {
		if p.Connected {
			connected++
		}
	}
	line("Devices: %d configured, %d connected", len(s.Peers), connected)
	for _, p := range s.Peers {
		if p.Connected {
			line("  %s  connected via %s  %s", Short(p.ID), p.Transport, redactAddr(p.Addr))
		} else {
			line("  %s  not connected", Short(p.ID))
		}
	}

	line("Folders: %d", len(s.Folders))
	for i, f := range s.Folders {
		st := f.State
		if st == "" {
			st = "unknown"
		}
		line("  #%d  state=%s paused=%t errors=%d need=%s (%s items) global=%s files=%s/%s",
			i+1, st, f.Paused, f.Errors, Size(f.NeedBytes), Grp(f.NeedItems), Size(f.GlobalBytes),
			Grp(f.LocalFiles), Grp(f.GlobalFiles))
	}

	line("Synced: %d%%  (%s)", s.Pct, s.SizeLine)
	if s.NeedItems > 0 {
		line("Pending: %s items, %s", Grp(s.NeedItems), Size(s.NeedBytes))
	}
	speed := "idle"
	if s.Moving {
		speed = "down " + Rate(s.InRate) + ", up " + Rate(s.OutRate)
	}
	if s.ETA != "" {
		speed += "  (ETA " + s.ETA + ")"
	}
	line("Speed: %s", speed)
	managed := ""
	if s.Startup.SyncthingManagedByUs {
		managed = " (Syncthing entry managed by " + brand.DisplayName + ")"
	}
	line("Startup: Syncthing %s / Tray %s%s", onOff(s.Startup.Syncthing), onOff(s.Startup.Tray), managed)
	if s.GUIURL != "" {
		line("Web UI: %s", RedactText(s.GUIURL))
	}
	if s.UpdateAvailable != "" {
		line("Update available: %s", s.UpdateAvailable)
	}

	if len(s.Pending) > 0 {
		line("Pending devices: %d", len(s.Pending))
		for _, p := range s.Pending {
			line("  %s  verified=%t  %s", Short(p.DeviceID), p.Verified, redactAddr(p.Addr.String()))
		}
	}
	if len(s.PendingFolders) > 0 {
		line("Pending folders: %d", len(s.PendingFolders))
	}
	if len(s.Notices) > 0 {
		line("Notices: %s", strings.Join(s.Notices, ", "))
	}

	if len(s.Activity) > 0 {
		line("Recent activity:")
		for i, a := range s.Activity {
			if i == 10 {
				break
			}
			line("  %s  %-5s %s", a.At.Format("15:04:05"), a.Tint, redactActivity(a.Text))
		}
	}

	if len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for k := range extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			line("%s: %s", RedactText(k), RedactText(extra[k]))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// Short returns the first 7 characters of a device ID (F8).
func Short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

var (
	deviceIDRe = regexp.MustCompile(`\b[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}\b|\b[A-Z2-7]{52,56}\b`)
	// Paths may contain spaces, so an unquoted path runs to the next ": "
	// separator (as in Go's "open <path>: <reason>" errors), a quote or the
	// end of the line. A path inside double quotes is replaced whole first.
	winPathRe  = regexp.MustCompile(`\b[A-Za-z]:[\\/](?:[^"<>|\r\n:]|:\S)*|\\\\[^\s"<>|](?:[^"<>|\r\n:]|:\S)*`)
	unixPathRe = regexp.MustCompile(`(^|[\s"'=(:,\[])(~?/[^/\s"'<>,;)\]](?:[^"<>\r\n:]|:\S)*)`)
	quotedRe   = regexp.MustCompile(`"[^"\r\n]*"`)
	ipv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	ipv6Re     = regexp.MustCompile(`\[?[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:%[0-9A-Za-z_.\-]+)?\]?`)
	keptPrefix = netip.MustParsePrefix("100.64.0.0/10")
)

// homeDir is the user's home directory, whose path and final element (the
// account name, often the user's real name) are removed from diagnostics as
// a second line of defence behind the path patterns. A variable for tests.
var homeDir = osutil.HomeDir

// RedactText removes secrets and identifying data from free text: registered
// secrets and API key patterns, full device IDs (cut to 7 characters), file
// system paths (including the rest of a path after a space), the home
// directory and account name, and IP addresses other than loopback and
// 100.64.0.0/10.
func RedactText(s string) string {
	s = applog.Redact(s)
	s = deviceIDRe.ReplaceAllStringFunc(s, func(id string) string { return id[:7] })
	s = quotedRe.ReplaceAllStringFunc(s, func(q string) string {
		if hasPath(q[1 : len(q)-1]) {
			return `"<path>"`
		}
		return q
	})
	s = winPathRe.ReplaceAllString(s, "<path>")
	s = redactUnixPaths(s)
	s = redactHome(s)
	s = ipv4Re.ReplaceAllStringFunc(s, redactIP)
	s = ipv6Re.ReplaceAllStringFunc(s, redactIP)
	return s
}

// hasPath reports whether s contains a Windows, UNC or Unix path (a Syncthing
// REST endpoint such as /rest/db/status does not count).
func hasPath(s string) bool {
	return winPathRe.MatchString(s) || redactUnixPaths(s) != s
}

// redactUnixPaths replaces Unix and home-relative paths with <path>. A
// Syncthing REST endpoint (/rest/...) is kept, up to its first space, and
// the text after it is scanned again.
func redactUnixPaths(s string) string {
	return unixPathRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := unixPathRe.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], "/rest/") {
			if i := strings.IndexAny(sub[2], " \t"); i >= 0 {
				return sub[1] + sub[2][:i] + redactUnixPaths(sub[2][i:])
			}
			return m
		}
		return sub[1] + "<path>"
	})
}

// redactHome removes any remaining occurrence of the home directory and, as
// a whole word in any letter case, the account name it ends in.
func redactHome(s string) string {
	home, err := homeDir()
	if err != nil || home == "" {
		return s
	}
	for _, h := range []string{home, filepath.ToSlash(home)} {
		s = strings.ReplaceAll(s, h, "<path>")
	}
	user := filepath.Base(home)
	if len(user) < 2 || user == "." || user == string(filepath.Separator) {
		return s
	}
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(user))
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(s, -1) {
		if wordRune(s[:m[0]], true) || wordRune(s[m[1]:], false) {
			continue // part of a longer word
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("<user>")
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// wordRune reports whether the rune at the end (before) or start (!before)
// of s is a letter, digit or underscore.
func wordRune(s string, before bool) bool {
	var r rune
	if before {
		r, _ = utf8.DecodeLastRuneInString(s)
	} else {
		r, _ = utf8.DecodeRuneInString(s)
	}
	return r != utf8.RuneError && (r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
}

func redactIP(m string) string {
	ip, err := netip.ParseAddr(strings.Trim(m, "[]"))
	if err != nil {
		return m
	}
	ip = ip.WithZone("").Unmap()
	if ip.IsLoopback() || keptPrefix.Contains(ip) {
		return m
	}
	return "<ip>"
}

// redactAddr keeps a peer address only when its host is loopback or tailnet.
func redactAddr(addr string) string {
	if addr == "" || addr == "invalid AddrPort" {
		return ""
	}
	ip, ok := hostIP(addr)
	if !ok {
		return "<address>"
	}
	if ip.IsLoopback() || keptPrefix.Contains(ip) {
		return addr
	}
	return "<ip>"
}

// activityRedactions maps the prefixes of Recent Activity lines that end in
// a file name, device or host name, or folder label to the placeholder that
// replaces the rest of the line.
var activityRedactions = []struct{ prefix, placeholder string }{
	{"Received ", "<file>"},
	{"Updated ", "<file>"},
	{"Deleted ", "<file>"},
	{"Failed: ", "<file>"},
	{"Local change: ", "<file>"},
	{"Remote change: ", "<file>"},
	{"Pairing requested: ", "<name>"},
	{"Paired with ", "<name>"},
	{"Shared ", "<name>"},
	{"Accepted ", "<name>"},
	{"Declined ", "<name>"},
}

// redactActivity hides the file names, device names and folder labels in
// Recent Activity lines.
func redactActivity(text string) string {
	for _, r := range activityRedactions {
		if strings.HasPrefix(text, r.prefix) {
			return r.prefix + r.placeholder
		}
	}
	return RedactText(text)
}
