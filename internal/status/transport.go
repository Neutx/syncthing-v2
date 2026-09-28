package status

import (
	"net/netip"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// Transport values reported for connected peers.
const (
	TransportTailscale model.Transport = "Tailscale"
	TransportRelay     model.Transport = "relay"
	TransportLAN       model.Transport = "local network"
	TransportDirect    model.Transport = "direct"
)

var (
	tailnetPrefixes = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
	}
	lanPrefixes = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
	}
)

// Classify names how a peer is connected, from the connection's remote
// address and type as reported by /rest/system/connections. The first
// matching rule wins:
//
//  1. the type contains "relay" → relay (even when the relay is reached over Tailscale)
//  2. the host is in 100.64.0.0/10 or fd7a:115c:a1e0::/48 → Tailscale
//  3. the host is private or link-local (10/8, 172.16/12, 192.168/16,
//     169.254/16, fc00::/7, fe80::/10) → local network
//  4. otherwise → direct
func Classify(addr, connType string) model.Transport {
	if strings.Contains(strings.ToLower(connType), "relay") {
		return TransportRelay
	}
	ip, ok := hostIP(addr)
	if !ok {
		return TransportDirect
	}
	for _, p := range tailnetPrefixes {
		if p.Contains(ip) {
			return TransportTailscale
		}
	}
	for _, p := range lanPrefixes {
		if p.Contains(ip) {
			return TransportLAN
		}
	}
	return TransportDirect
}

// hostIP extracts the IP of an address such as "100.64.0.1:22000",
// "[fe80::1%eth0]:22000", "tcp://192.0.2.1:22000" or a bare IP. The zone is
// dropped and IPv4-mapped IPv6 addresses are unmapped.
func hostIP(addr string) (netip.Addr, bool) {
	a := strings.TrimSpace(addr)
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	a = strings.TrimSuffix(a, "/")
	var ip netip.Addr
	if ap, err := netip.ParseAddrPort(a); err == nil {
		ip = ap.Addr()
	} else if p, err := netip.ParseAddr(strings.Trim(a, "[]")); err == nil {
		ip = p
	} else {
		return netip.Addr{}, false
	}
	return ip.WithZone("").Unmap(), true
}
