//go:build !windows

package install

import "context"

// AllowFirewall is Windows only; on macOS and Linux the docs describe the
// application firewall prompt and `ufw allow in on tailscale0 to any port
// 22000`.
func AllowFirewall(context.Context, string) error { return ErrFirewallUnsupported }

// AllowFirewallElevated is Windows only.
func AllowFirewallElevated(context.Context, string) error { return ErrFirewallUnsupported }

// HasFirewallRule is Windows only.
func HasFirewallRule(context.Context) (bool, error) { return false, ErrFirewallUnsupported }
