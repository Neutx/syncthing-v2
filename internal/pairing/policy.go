package pairing

import (
	"net/netip"

	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

// Decision actions.
const (
	ActionPrompt = "prompt"
	ActionIgnore = "ignore"
)

// Decision reasons.
const (
	ReasonNotTailnet    = "not from tailnet"
	ReasonUnknownNode   = "unknown tailnet node"
	ReasonMismatch      = "identity mismatch"
	ReasonProbeFailed   = "identity not verified"
	ReasonOtherOwner    = "other owner"
	ReasonSameOwner     = "same owner"
	ReasonNoPendingAddr = "no address"
	ReasonNoPendingID   = "no device ID"
	ReasonSelf          = "this computer"
)

// Policy decides which pending devices may be shown as pairing prompts.
// Prefixes is tailnet.Prefixes in production; the e2e harness passes
// 127.0.0.0/8.
type Policy struct {
	Prefixes []netip.Prefix
}

// Decision is the verdict on one pending device. Node is set whenever the
// pending address belongs to a tailnet node.
type Decision struct {
	Action string // "prompt" or "ignore"
	Reason string
	Node   *tailnet.Node
}

// Decide is a pure function of its inputs:
//
//  1. the pending address must be inside Prefixes, else ignore ("not from tailnet");
//  2. an address of this computer itself is ignored ("this computer"): any
//     local process or account can connect from it, so it proves nothing;
//  3. a peer in ts must own that address, else ignore ("unknown tailnet node");
//  4. the probe of that address must have succeeded and returned exactly the
//     pending device ID, else ignore ("identity not verified" or "identity mismatch");
//  5. otherwise prompt, with reason "same owner" or "other owner".
func (p Policy) Decide(pd model.PendingDevice, ts tailnet.Status, probedID string, probeErr error) Decision {
	if pd.DeviceID == "" {
		return Decision{Action: ActionIgnore, Reason: ReasonNoPendingID}
	}
	if !pd.Addr.IsValid() {
		return Decision{Action: ActionIgnore, Reason: ReasonNoPendingAddr}
	}
	ip := pd.Addr.Addr().Unmap().WithZone("")
	if !p.contains(ip) {
		return Decision{Action: ActionIgnore, Reason: ReasonNotTailnet}
	}
	if ts.SelfHasIP(ip) {
		self := ts.Self
		return Decision{Action: ActionIgnore, Reason: ReasonSelf, Node: &self}
	}
	n, ok := ts.FindByIP(ip)
	if !ok {
		return Decision{Action: ActionIgnore, Reason: ReasonUnknownNode}
	}
	node := &n
	if probeErr != nil || probedID == "" {
		return Decision{Action: ActionIgnore, Reason: ReasonProbeFailed, Node: node}
	}
	if probedID != pd.DeviceID {
		return Decision{Action: ActionIgnore, Reason: ReasonMismatch, Node: node}
	}
	if ts.SameOwner(n) {
		return Decision{Action: ActionPrompt, Reason: ReasonSameOwner, Node: node}
	}
	return Decision{Action: ActionPrompt, Reason: ReasonOtherOwner, Node: node}
}

func (p Policy) contains(ip netip.Addr) bool {
	for _, pfx := range p.Prefixes {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

// candidateFor converts a tailnet node into the model's Candidate.
func candidateFor(ts tailnet.Status, n tailnet.Node) model.Candidate {
	ip, _ := n.PreferredIP()
	return model.Candidate{
		NodeName:  n.HostName,
		DNSName:   n.DNSName,
		OS:        n.OS,
		LoginName: n.LoginName,
		IP:        ip,
		SameOwner: ts.SameOwner(n),
	}
}
