package stclient

import (
	"net/netip"
	"strings"
)

// Finding is one security audit result. Code is a doctor code (§3.11) and
// Severity is "high", "warn" or "info".
type Finding struct {
	Code, Severity, Text string
}

// Audit checks the GUI settings of an adopted Syncthing. It reports SEC001
// (high) when the GUI is reachable beyond loopback without a password, or when
// insecureAdminAccess is on. A secure config yields no findings.
func Audit(g GUIConfig) []Finding {
	var reasons []string
	if !isLoopbackListen(g.Address) && !g.HasUser {
		reasons = append(reasons, "The Syncthing control panel listens on "+displayAddress(g)+", which other devices can reach, and it has no password.")
	}
	if g.InsecureAdminAccess {
		reasons = append(reasons, "insecureAdminAccess is enabled, which turns off the control panel's host and password checks.")
	}
	if len(reasons) == 0 {
		return nil
	}
	return []Finding{{Code: "SEC001", Severity: "high", Text: strings.Join(reasons, " ")}}
}

// isLoopbackListen reports whether a GUI listen address accepts connections
// from this machine only. Wildcards, LAN addresses and host names other than
// "localhost" count as reachable from other devices.
func isLoopbackListen(addr string) bool {
	h, _, err := splitAddress(addr)
	if err != nil {
		return true // a Unix socket is local by nature
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

func displayAddress(g GUIConfig) string {
	if strings.TrimSpace(g.Address) == "" {
		return "all network interfaces"
	}
	return strings.TrimSpace(g.Address)
}
