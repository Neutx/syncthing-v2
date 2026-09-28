package pairing

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

func TestDecideTruthTable(t *testing.T) {
	ts := testTailnet()
	p := Policy{Prefixes: tailnet.Prefixes}
	pending := func(id, addr string) model.PendingDevice {
		return model.PendingDevice{DeviceID: id, Name: "remote", Addr: netip.MustParseAddrPort(addr)}
	}
	probeErr := errors.New("probe failed")

	cases := []struct {
		name     string
		pd       model.PendingDevice
		probedID string
		probeErr error
		action   string
		reason   string
		node     string // expected Node.HostName, "" for none
	}{
		{"non-tailnet IPv4 (LAN)", pending(idLAN, "192.168.1.50:22000"), idLAN, nil, ActionIgnore, ReasonNotTailnet, ""},
		{"non-tailnet public IP", pending(idLAN, "203.0.113.7:4000"), idLAN, nil, ActionIgnore, ReasonNotTailnet, ""},
		{"non-tailnet IPv6", pending(idLAN, "[2001:db8::1]:22000"), idLAN, nil, ActionIgnore, ReasonNotTailnet, ""},
		{"unknown tailnet node", pending(idOther, "100.64.0.99:22000"), idOther, nil, ActionIgnore, ReasonUnknownNode, ""},
		{"probe mismatch (SNAT)", pending(idOther, "100.64.0.2:51000"), idLaptop, nil, ActionIgnore, ReasonMismatch, "laptop"},
		{"probe error", pending(idLaptop, "100.64.0.2:51000"), "", probeErr, ActionIgnore, ReasonProbeFailed, "laptop"},
		{"probe error with an ID", pending(idLaptop, "100.64.0.2:51000"), idLaptop, probeErr, ActionIgnore, ReasonProbeFailed, "laptop"},
		{"empty probe result", pending(idLaptop, "100.64.0.2:51000"), "", nil, ActionIgnore, ReasonProbeFailed, "laptop"},
		{"same owner", pending(idLaptop, "100.64.0.2:51000"), idLaptop, nil, ActionPrompt, ReasonSameOwner, "laptop"},
		{"same owner over IPv6", pending(idMac, "[fd7a:115c:a1e0::3]:51000"), idMac, nil, ActionPrompt, ReasonSameOwner, "mac"},
		{"same owner, 4in6 address", pending(idLaptop, "[::ffff:100.64.0.2]:51000"), idLaptop, nil, ActionPrompt, ReasonSameOwner, "laptop"},
		{"other owner", pending(idFriend, "100.64.0.8:51000"), idFriend, nil, ActionPrompt, ReasonOtherOwner, "friend-pc"},
		{"no address", model.PendingDevice{DeviceID: idLaptop}, idLaptop, nil, ActionIgnore, ReasonNoPendingAddr, ""},
		{"no device ID", pending("", "100.64.0.2:51000"), idLaptop, nil, ActionIgnore, ReasonNoPendingID, ""},
	}
	for _, tc := range cases {
		d := p.Decide(tc.pd, ts, tc.probedID, tc.probeErr)
		if d.Action != tc.action || d.Reason != tc.reason {
			t.Errorf("%s: Decide = %s/%q, want %s/%q", tc.name, d.Action, d.Reason, tc.action, tc.reason)
		}
		switch {
		case tc.node == "" && d.Node != nil:
			t.Errorf("%s: Node = %+v, want nil", tc.name, d.Node)
		case tc.node != "" && (d.Node == nil || d.Node.HostName != tc.node):
			t.Errorf("%s: Node = %+v, want %s", tc.name, d.Node, tc.node)
		}
	}
}

func TestDecideOtherOwnerCarriesLogin(t *testing.T) {
	d := Policy{Prefixes: tailnet.Prefixes}.Decide(
		model.PendingDevice{DeviceID: idFriend, Addr: netip.MustParseAddrPort("100.64.0.8:1")},
		testTailnet(), idFriend, nil)
	if d.Action != ActionPrompt || d.Node == nil || d.Node.LoginName != "friend@example.org" || d.Node.UserID == testTailnet().Self.UserID {
		t.Fatalf("Decide = %+v", d)
	}
	c := candidateFor(testTailnet(), *d.Node)
	if c.SameOwner || c.LoginName != "friend@example.org" || c.IP != netip.MustParseAddr("100.64.0.8") {
		t.Fatalf("candidate = %+v", c)
	}
}

func TestDecideUnknownOwnerIsNotSameOwner(t *testing.T) {
	ts := testTailnet()
	ts.Self.UserID = 0
	ts.Peers[0].UserID = 0
	d := Policy{Prefixes: tailnet.Prefixes}.Decide(
		model.PendingDevice{DeviceID: idLaptop, Addr: netip.MustParseAddrPort("100.64.0.2:1")}, ts, idLaptop, nil)
	if d.Action != ActionPrompt || d.Reason != ReasonOtherOwner {
		t.Fatalf("zero UserIDs: %+v; want a prompt with the other-owner warning", d)
	}
}

func TestDecideCustomPrefixes(t *testing.T) {
	// The e2e harness runs on loopback with Policy{Prefixes: 127.0.0.0/8}.
	ts := tailnet.Status{
		BackendState: "Running",
		Self:         tailnet.Node{HostName: "a", UserID: 1, IPs: []netip.Addr{netip.MustParseAddr("127.0.0.2")}},
		Peers:        []tailnet.Node{{HostName: "b", UserID: 1, IPs: []netip.Addr{netip.MustParseAddr("127.0.0.3")}}},
	}
	pd := model.PendingDevice{DeviceID: idLaptop, Addr: netip.MustParseAddrPort("127.0.0.3:40000")}
	loop := Policy{Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	if d := loop.Decide(pd, ts, idLaptop, nil); d.Action != ActionPrompt {
		t.Fatalf("loopback policy: %+v", d)
	}
	if d := (Policy{Prefixes: tailnet.Prefixes}).Decide(pd, ts, idLaptop, nil); d.Reason != ReasonNotTailnet {
		t.Fatalf("production policy on loopback: %+v", d)
	}
	if d := (Policy{}).Decide(pd, ts, idLaptop, nil); d.Action != ActionIgnore {
		t.Fatalf("empty policy must ignore everything: %+v", d)
	}
}
