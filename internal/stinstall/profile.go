package stinstall

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// Profile is a transport profile (§4.5).
type Profile int

const (
	// Tailnet turns off global discovery, relays and NAT traversal and keeps
	// local discovery: sync runs over Tailscale or the local network only.
	// It is applied once to configs SyncThing V2 generates.
	Tailnet Profile = iota
	// Hybrid restores upstream's defaults: global discovery, relays and NAT
	// traversal on.
	Hybrid
)

func (p Profile) String() string {
	switch p {
	case Tailnet:
		return "tailnet"
	case Hybrid:
		return "hybrid"
	}
	return fmt.Sprintf("Profile(%d)", int(p))
}

// ParseProfile parses "tailnet" or "hybrid" (case-insensitive).
func ParseProfile(s string) (Profile, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "tailnet":
		return Tailnet, nil
	case "hybrid":
		return Hybrid, nil
	}
	return 0, fmt.Errorf("unknown profile %q (want tailnet or hybrid)", s)
}

// profileOptions is the subset of /rest/config/options a profile sets.
type profileOptions struct {
	GlobalAnnounceEnabled bool     `json:"globalAnnounceEnabled"`
	RelaysEnabled         bool     `json:"relaysEnabled"`
	NATEnabled            bool     `json:"natEnabled"`
	LocalAnnounceEnabled  bool     `json:"localAnnounceEnabled"`
	ListenAddresses       []string `json:"listenAddresses"`
}

func optionsFor(p Profile) (profileOptions, error) {
	switch p {
	case Tailnet:
		return profileOptions{LocalAnnounceEnabled: true, ListenAddresses: []string{"default"}}, nil
	case Hybrid:
		return profileOptions{GlobalAnnounceEnabled: true, RelaysEnabled: true, NATEnabled: true,
			LocalAnnounceEnabled: true, ListenAddresses: []string{"default"}}, nil
	}
	return profileOptions{}, fmt.Errorf("unknown profile %d", int(p))
}

// ApplyProfile sets the options of profile p with one PATCH of
// /rest/config/options (other options are left alone), then reads them back
// to confirm Syncthing stored them.
func ApplyProfile(ctx context.Context, c *stclient.Client, p Profile) error {
	want, err := optionsFor(p)
	if err != nil {
		return err
	}
	if err := c.Send(ctx, http.MethodPatch, "/rest/config/options", want); err != nil {
		return fmt.Errorf("apply %s profile: %w", p, err)
	}
	got, ok, err := CurrentProfile(ctx, c)
	if err != nil {
		return fmt.Errorf("apply %s profile: read back options: %w", p, err)
	}
	if !ok || got != p {
		return fmt.Errorf("apply %s profile: Syncthing did not keep the new options", p)
	}
	return nil
}

// CurrentProfile reads /rest/config/options and reports which profile the
// options match. ok is false when they match neither (a custom setup).
func CurrentProfile(ctx context.Context, c *stclient.Client) (p Profile, ok bool, err error) {
	var cur profileOptions
	if err := c.Get(ctx, "/rest/config/options", &cur); err != nil {
		return 0, false, err
	}
	for _, cand := range []Profile{Tailnet, Hybrid} {
		o, _ := optionsFor(cand)
		if cur.GlobalAnnounceEnabled == o.GlobalAnnounceEnabled &&
			cur.RelaysEnabled == o.RelaysEnabled &&
			cur.NATEnabled == o.NATEnabled &&
			cur.LocalAnnounceEnabled == o.LocalAnnounceEnabled &&
			slices.Equal(cur.ListenAddresses, o.ListenAddresses) {
			return cand, true, nil
		}
	}
	return 0, false, nil
}
