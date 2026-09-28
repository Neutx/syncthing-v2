# Security policy

## Reporting a vulnerability

Please **do not** open a public issue for a security problem.

Report it privately through a GitHub security advisory: **[Report a vulnerability](https://github.com/Neutx/syncthing-v2/security/advisories/new)** (repository **Security** tab → **Advisories** → **Report a vulnerability**). Include these details:

- the SyncThing V2 version (`stv2 version`) and your OS
- what an attacker can do, and what they need first. For example: another user on the same tailnet, a website open in your browser, or local access.
- steps to reproduce, or a proof of concept
- any logs or output, with your own device IDs, addresses and names removed

The maintainers aim to acknowledge a report within a week. You can follow the fix in the private advisory, and you will be credited in the advisory and the changelog unless you ask otherwise. Please give us a reasonable time to release a fix before you disclose the issue publicly.

## Supported versions

Security fixes go into the **latest minor release** (for example, all 1.0.x once 1.0.0 is out). Please check that the issue still exists in the latest release before you report it. Older minor releases do not get fixes. Upgrading is a matter of running the one-line installer again.

| Version | Supported |
|---|---|
| Latest minor release | Yes |
| Older releases | No |

## Scope

**In scope:**

- the `stv2` program on all platforms: the tray, the loopback dashboard server and its authentication, the pairing logic and identity checks, the Syncthing download and extraction, install and uninstall, `stv2 doctor` and the other commands
- the install scripts (`install.ps1`, `install.sh`) and the release assets and pipeline (`.github/workflows/`)
- anything that leaks the Syncthing API key, pairs or accepts without consent, or writes outside the intended folders

**Out of scope** (please report these elsewhere):

- vulnerabilities in **Syncthing** itself: see [syncthing.net/security](https://syncthing.net/security/)
- vulnerabilities in **Tailscale**: see [tailscale.com/security](https://tailscale.com/security)
- attacks that need malware already running as your user, or administrator access to your computer
- the SmartScreen and Gatekeeper warnings on unsigned 1.0 builds (documented, and expected)
- findings from automated scanners without a demonstrated impact

The threat model, and what SyncThing V2 does and does not expose, are described in [docs/security.md](docs/security.md).
