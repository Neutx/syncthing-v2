package status

import (
	"fmt"
	"net/netip"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/model"
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
	winPathRe  = regexp.MustCompile(`\b[A-Za-z]:[\\/][^\s"'<>|,;]*|\\\\[^\s"'<>|,;]+`)
	unixPathRe = regexp.MustCompile(`(^|[\s"'=(:,\[])(~?/[^/\s"'<>,;)\]][^\s"'<>,;)\]]*)`)
	ipv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	ipv6Re     = regexp.MustCompile(`\[?[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:%[0-9A-Za-z_.\-]+)?\]?`)
	keptPrefix = netip.MustParsePrefix("100.64.0.0/10")
)

// RedactText removes secrets and identifying data from free text: registered
// secrets and API key patterns, full device IDs (cut to 7 characters), file
// system paths, and IP addresses other than loopback and 100.64.0.0/10.
func RedactText(s string) string {
	s = applog.Redact(s)
	s = deviceIDRe.ReplaceAllStringFunc(s, func(id string) string { return id[:7] })
	s = winPathRe.ReplaceAllString(s, "<path>")
	s = unixPathRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := unixPathRe.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], "/rest/") {
			return m
		}
		return sub[1] + "<path>"
	})
	s = ipv4Re.ReplaceAllStringFunc(s, redactIP)
	s = ipv6Re.ReplaceAllStringFunc(s, redactIP)
	return s
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

// redactActivity hides the file names in Recent Activity lines.
func redactActivity(text string) string {
	for _, p := range []string{"Received ", "Updated ", "Deleted ", "Failed: ", "Local change: ", "Remote change: "} {
		if strings.HasPrefix(text, p) {
			return p + "<file>"
		}
	}
	return RedactText(text)
}
