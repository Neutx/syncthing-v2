package status

import (
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		addr, typ string
		want      model.Transport
	}{
		// Tailscale 100.64.0.0/10 edges.
		{"100.63.255.255:22000", "tcp-client", TransportDirect},
		{"100.64.0.0:22000", "tcp-client", TransportTailscale},
		{"100.64.0.1:22000", "tcp-client", TransportTailscale},
		{"100.127.255.255:22000", "tcp-server", TransportTailscale},
		{"100.128.0.0:22000", "tcp-client", TransportDirect},
		{"100.1.2.3:22000", "tcp-client", TransportDirect},
		{"[fd7a:115c:a1e0::1]:22000", "quic-client", TransportTailscale},
		{"[fd7a:115c:a1e1::1]:22000", "quic-client", TransportLAN}, // still fc00::/7, not the tailnet /48

		// Relay first, even over a tailnet or LAN address.
		{"100.64.0.1:22067", "relay-client", TransportRelay},
		{"192.168.1.5:22067", "relay-server", TransportRelay},
		{"198.51.100.4:22067", "Relay-Client", TransportRelay},

		// Private and link-local ranges.
		{"10.0.0.1:22000", "tcp-client", TransportLAN},
		{"10.255.255.255:22000", "tcp-client", TransportLAN},
		{"172.15.255.255:22000", "tcp-client", TransportDirect},
		{"172.16.0.1:22000", "tcp-client", TransportLAN},
		{"172.31.4.5:22000", "tcp-client", TransportLAN},
		{"172.32.0.1:22000", "tcp-client", TransportDirect},
		{"192.168.0.1:22000", "tcp-client", TransportLAN},
		{"192.169.0.1:22000", "tcp-client", TransportDirect},
		{"169.254.10.20:22000", "tcp-client", TransportLAN},
		{"[fc00::1]:22000", "quic-server", TransportLAN},
		{"[fdff:ffff::1]:22000", "quic-server", TransportLAN},
		{"[fe80::1%eth0]:22000", "tcp-client", TransportLAN},
		{"fe80::1%eth0", "tcp-client", TransportLAN},
		{"[fe80::1]:22000", "tcp-client", TransportLAN},
		{"[febf::1]:22000", "tcp-client", TransportLAN},
		{"[fec0::1]:22000", "tcp-client", TransportDirect},

		// Public, mapped and odd forms.
		{"198.51.100.7:22000", "tcp-client", TransportDirect},
		{"[2001:db8::1]:22000", "quic-client", TransportDirect},
		{"[::ffff:100.64.0.9]:22000", "tcp-client", TransportTailscale},
		{"tcp://100.64.0.2:22000", "tcp-client", TransportTailscale},
		{"10.1.2.3", "tcp-client", TransportLAN},
		{"", "", TransportDirect},
		{"not-an-address", "tcp-client", TransportDirect},
	}
	for _, c := range cases {
		if got := Classify(c.addr, c.typ); got != c.want {
			t.Errorf("Classify(%q, %q) = %q, want %q", c.addr, c.typ, got, c.want)
		}
	}
}
