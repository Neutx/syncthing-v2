// Package tailnet reads the local Tailscale state through the tailscale CLI
// (`tailscale status --json`). It never changes Tailscale settings. The
// Source interface lets tests and the e2e harness supply a fake tailnet.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// StatusTimeout bounds one `tailscale status --json` call.
const StatusTimeout = 5 * time.Second

// BackendRunning is the BackendState of a connected, logged-in client.
const BackendRunning = "Running"

// Prefixes are the address ranges Tailscale assigns to nodes: the CGNAT
// range and Tailscale's IPv6 ULA prefix.
var Prefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// Source reports the current tailnet status. Production uses CLI; tests use a fake.
type Source interface {
	Status(ctx context.Context) (Status, error)
}

// Node is one machine on the tailnet.
type Node struct {
	ID, HostName, DNSName, OS, LoginName string
	IPs                                  []netip.Addr
	Online, Sharee                       bool
	Tags                                 []string
	UserID                               int64
}

// IPv4 returns the node's first IPv4 address, if it has one.
func (n Node) IPv4() (netip.Addr, bool) {
	for _, ip := range n.IPs {
		if ip.Is4() {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

// PreferredIP returns the node's IPv4 address, or its first address when it
// has no IPv4 address. ok is false when the node has no address at all.
func (n Node) PreferredIP() (netip.Addr, bool) {
	if ip, ok := n.IPv4(); ok {
		return ip, true
	}
	if len(n.IPs) > 0 {
		return n.IPs[0], true
	}
	return netip.Addr{}, false
}

// Status is the parsed output of `tailscale status --json`.
type Status struct {
	BackendState string
	Self         Node
	Peers        []Node
}

// Running reports whether Tailscale is connected and logged in.
func (s Status) Running() bool { return s.BackendState == BackendRunning }

// FindByIP returns the node (self or a peer) that owns ip.
func (s Status) FindByIP(ip netip.Addr) (Node, bool) {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return Node{}, false
	}
	if hasIP(s.Self, ip) {
		return s.Self, true
	}
	for _, p := range s.Peers {
		if hasIP(p, ip) {
			return p, true
		}
	}
	return Node{}, false
}

func hasIP(n Node, ip netip.Addr) bool {
	for _, a := range n.IPs {
		if a == ip {
			return true
		}
	}
	return false
}

// SameOwner reports whether n belongs to the same Tailscale user as Self. A
// zero UserID means the owner is unknown and never counts as the same owner.
func (s Status) SameOwner(n Node) bool { return n.UserID != 0 && n.UserID == s.Self.UserID }

// Candidates returns the peers that can be offered for pairing: online,
// running Windows, macOS or Linux, not shared in from another tailnet and
// without ACL tags. Same-owner nodes come first; each group is sorted by
// host name.
func (s Status) Candidates() []Node {
	var out []Node
	for _, p := range s.Peers {
		if !p.Online || p.Sharee || len(p.Tags) > 0 || !desktopOS(p.OS) {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := s.SameOwner(out[i]), s.SameOwner(out[j])
		if si != sj {
			return si
		}
		hi, hj := strings.ToLower(out[i].HostName), strings.ToLower(out[j].HostName)
		if hi != hj {
			return hi < hj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func desktopOS(os string) bool {
	switch strings.ToLower(os) {
	case "windows", "macos", "linux":
		return true
	}
	return false
}

// CLI runs the tailscale command-line tool at Path.
type CLI struct{ Path string }

// Status runs `tailscale status --json` without a console window and with a
// 5 s timeout. The CLI exits non-zero in some states (for example when
// logged out) while still printing valid JSON; that JSON is used when present.
func (c CLI) Status(ctx context.Context) (Status, error) {
	if c.Path == "" {
		return Status{}, ErrNotInstalled
	}
	out, err := osutil.Output(ctx, StatusTimeout, c.Path, "status", "--json")
	if len(strings.TrimSpace(string(out))) > 0 {
		st, perr := Parse(out)
		if perr == nil {
			return st, nil
		}
		if err == nil {
			return Status{}, perr
		}
	}
	if err != nil {
		return Status{}, fmt.Errorf("tailscale status: %w", err)
	}
	return Status{}, errors.New("tailscale status: empty output")
}

type jsonNode struct {
	ID           string   `json:"ID"`
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	UserID       int64    `json:"UserID"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	ShareeNode   bool     `json:"ShareeNode"`
	Tags         []string `json:"Tags"`
}

type jsonUser struct {
	LoginName string `json:"LoginName"`
}

type jsonStatus struct {
	BackendState string              `json:"BackendState"`
	Self         *jsonNode           `json:"Self"`
	Peer         map[string]jsonNode `json:"Peer"`
	User         map[string]jsonUser `json:"User"`
}

// Parse decodes `tailscale status --json` output. Peers are returned in a
// stable order (by host name, then node ID).
func Parse(data []byte) (Status, error) {
	var js jsonStatus
	if err := json.Unmarshal(data, &js); err != nil {
		return Status{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	if js.BackendState == "" {
		return Status{}, errors.New("parse tailscale status: no BackendState")
	}
	st := Status{BackendState: js.BackendState}
	if js.Self != nil {
		st.Self = convert(*js.Self, js.User)
	}
	for _, p := range js.Peer {
		st.Peers = append(st.Peers, convert(p, js.User))
	}
	sort.Slice(st.Peers, func(i, j int) bool {
		if st.Peers[i].HostName != st.Peers[j].HostName {
			return st.Peers[i].HostName < st.Peers[j].HostName
		}
		return st.Peers[i].ID < st.Peers[j].ID
	})
	return st, nil
}

func convert(j jsonNode, users map[string]jsonUser) Node {
	n := Node{
		ID:       j.ID,
		HostName: j.HostName,
		DNSName:  strings.TrimSuffix(j.DNSName, "."),
		OS:       j.OS,
		Online:   j.Online,
		Sharee:   j.ShareeNode,
		Tags:     j.Tags,
		UserID:   j.UserID,
	}
	if u, ok := users[strconv.FormatInt(j.UserID, 10)]; ok {
		n.LoginName = u.LoginName
	}
	for _, s := range j.TailscaleIPs {
		if ip, err := netip.ParseAddr(s); err == nil {
			n.IPs = append(n.IPs, ip.Unmap())
		}
	}
	return n
}
