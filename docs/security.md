# Security

This page describes what SyncThing V2 exposes, what it protects against and how. To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Principles

- **No new listeners, ports or secrets on the wire.** SyncThing V2 opens no network port that other machines can reach. Identity comes from three places:
  1. **Tailscale.** WireGuard ties a `100.x` source address to a device and its owner.
  2. **Syncthing.** Mutual TLS ties a device ID to its key.
  3. **You.** A click on each computer.
- **Consent on both sides.** Nothing is accepted without a click on that computer: no device and no folder. Syncthing's `autoAcceptFolders` stays off for devices that SyncThing V2 adds.
- **Your existing Syncthing is not changed silently.** An adopted Syncthing's binary, configuration and autostart are only read. They change only through actions you confirm: fixing GUI exposure, changing the profile, pairing and sharing.
- **No shell anywhere.** Every external program is started with an argument list. Host names, folder labels and file names are never put into a command line or script.

## What is exposed, and what is not

| Component | Listens on | Reachable from |
|---|---|---|
| SyncThing V2 dashboard server | `127.0.0.1:<random port>` | This computer only, and only with a session (see below) |
| Syncthing sync protocol (Syncthing's own) | `:22000` TCP and UDP by default | The network. This is what Syncthing needs in order to sync. Every connection is mutual TLS between known device IDs. |
| Syncthing web UI and REST API (Syncthing's own) | `127.0.0.1:8384` by default | This computer only, in configs that SyncThing V2 generates |

SyncThing V2 makes these outbound connections:

- `tailscale status --json` (local)
- the Syncthing REST API on loopback
- TLS probes to port 22000 of tailnet devices (only when you open the Pair view or click Refresh, plus once at startup)
- one GitHub API request a day for the update check
- the Syncthing download from GitHub Releases, only when Syncthing is missing

## Threat model

| # | Threat | Control |
|---|---|---|
| T1 | Another tailnet user's device pairs silently | There is no auto-accept at all. A different owner gets a warned prompt that shows their Tailscale login and the short device ID. Shared-in and tagged devices are excluded from discovery. |
| T2 | A spoofed device ID or source address, for example source NAT through a subnet router or exit node | Syncthing's mutual TLS proves the pending device ID. SyncThing V2's probe proves that the device at that `100.x` address holds that ID. The tailnet lookup proves that the address belongs to a known device. A request that fails any check is never shown. |
| T3 | Pending devices from the LAN, relays or the internet | SyncThing V2 never handles them. They are left to Syncthing's web UI. |
| T4 | Probe side effects, such as filling other computers with pending requests | The probe aborts the TLS handshake as soon as it has read the peer's certificate, before sending one of its own, so the peer never records a request. An end-to-end test checks this against real Syncthing. Probes run only on user action and once at startup. |
| T5 | Syncthing's control panel exposed on the network | Configs that SyncThing V2 generates keep upstream's loopback default. An adopted config that is reachable from the network without a password, or with `insecureAdminAccess`, raises a one-time notice ("Restrict it to this computer?") and doctor code [SEC001](troubleshooting.md#sec001). It is never changed without your click. |
| T6 | Local web attacks on the dashboard server: DNS rebinding, CSRF, cross-origin reads | Host and Origin checks, `Sec-Fetch-Site` checks, a `SameSite=Strict; HttpOnly` session cookie, a custom header on POSTs, and no CORS headers (see below). |
| T7 | The dashboard login token leaking through the process list | On Windows the token goes to the dashboard host over stdin. On macOS and Linux it sits in a 0600 file in a 0700 directory that is deleted after 30 s. Tokens are single-use, expire after 30 s and are new on every launch. |
| T8 | The Syncthing API key leaking | It is never sent to the dashboard page or a browser, and never logged. The log redacts the key and any `X-API-Key` header. "Copy diagnostics" leaves it out. |
| T9 | Path traversal from a remote folder label or from a download archive | Folder labels are cleaned: safe characters only, no Windows reserved names, a length cap, and the result must stay under the base folder. Archive extraction rejects absolute paths, `..`, and links that point outside the target. |
| T10 | Notification or command injection through host or file names | All programs run with argument lists. The macOS notification script receives its text as `argv`, never as script source. |
| T11 | A tampered Syncthing download | SHA-256 pins are compiled into the binary. They are generated from upstream's `sha256sum.txt.asc` after its GPG signature is checked against Syncthing's release key (`build/syncthing-release-key.asc`). A mismatch deletes the file and aborts. |
| T12 | A compromised release pipeline | GitHub Actions are pinned to commit SHAs, and workflows run with `contents: read` by default. Builds are reproducible (`-trimpath`, fixed `SOURCE_DATE_EPOCH`) and carry build provenance attestations and a `SHA256SUMS.txt`. Releases are drafts until a maintainer publishes them. |
| T13 | A tampered one-line install | The install scripts check `SHA256SUMS.txt` before running anything. The scripts are release assets themselves, so each release has its own versioned copy. You can check them with `gh attestation verify`. |
| T14 | Maintainer data leaking into the public repository | A private denylist check runs in CI and in the pre-commit hook, together with gitleaks. Test fixtures and screenshots use synthetic data only. |
| T15 | Privacy of the update check | One unauthenticated GET to `api.github.com` per 24 h, with no identifiers beyond the `stv2/<version>` user agent. You can turn it off in Settings. |
| — | Out of scope | Malware already running as your user, which could read Syncthing's configuration anyway. A compromised Tailscale control plane. |

## The dashboard server

The dashboard is served by the tray process itself:

- **Listener:** `127.0.0.1:0` only, a random port on loopback.
- **Host check:** every request must carry `Host: 127.0.0.1:<port>`. Anything else is refused, which defeats DNS rebinding.
- **Origin check:** an `Origin` header, when present, must be `http://127.0.0.1:<port>`. The only exception is `Origin: null` on `/login` together with a valid token (the launch file). Requests the browser marks as cross-site or same-site are refused everywhere except `/login`.
- **Session:** `/login` exchanges a single-use, 30-second launch token for the `stv2s` cookie. The tray's launch page sends the token in a form POST. `GET /login?token=…` is also accepted, with the same single-use and 30-second rules, for the links that `stv2 demo` and `/api/launch` hand out; the response carries `Referrer-Policy: no-referrer`, so the token does not leak to other sites. The cookie holds a random 256-bit value, with `HttpOnly; SameSite=Strict; Path=/`. Sessions live in memory only and end when the tray exits.
- **POSTs** also need the header `X-STV2: 1`, which a cross-site form cannot set.
- **Headers:** no CORS headers, ever. Every response carries:
  - `Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'`
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: no-referrer`
  - `X-Frame-Options: DENY`
- **Control endpoints** (`/api/show`, `/api/quit`, `/api/launch`) accept only `POST` with `Authorization: Bearer <control token>` and `X-STV2: 1`. That token is stored in `instance.json` (mode 0600) in your data directory.

## Supply chain

- **Syncthing** is downloaded only when missing, from `github.com/syncthing/syncthing/releases`, at the version pinned in the binary. The SHA-256 is checked before extraction. The upstream `LICENSE.txt`, `README.txt` and `AUTHORS.txt` are kept next to it. Syncthing's own auto-upgrade stays on and does its own signature checks.
- **SyncThing V2 releases** are built by `.github/workflows/release.yml` from a tag. Every asset has an entry in `SHA256SUMS.txt` and a build provenance attestation. To verify:

  ```sh
  gh attestation verify <file> --repo Neutx/syncthing-v2
  ```

- **Go modules** are pinned in `go.sum`. Dependabot proposes updates weekly, and `govulncheck` runs in CI.
- Release 1.0 binaries are not Authenticode-signed or notarized. The pipeline has conditional signing steps, ready for when certificates are available.

## Privacy

- **Update check.** Once a day, SyncThing V2 asks `https://api.github.com/repos/Neutx/syncthing-v2/releases/latest` for the latest release. The request carries no cookies, tokens or identifiers except the `stv2/<version>` user agent, and it shares your IP address with GitHub like any web request. Turn it off in **Settings → Check for updates**. It then never runs, and `stv2 doctor` skips [UPD001](troubleshooting.md#upd001).
- **Diagnostics.** "Copy diagnostics" puts a redacted report on the clipboard. It has no API key, device IDs are cut to 7 characters, and there are no file paths. Only tailnet (`100.64.0.0/10`) addresses are kept. Nothing is sent anywhere unless you paste it.
- **Logs** stay on your computer (see [troubleshooting.md](troubleshooting.md#logs-and-diagnostics)).
- **No telemetry.** SyncThing V2 collects no usage data. Syncthing's own anonymous usage reporting is controlled by Syncthing, which asks you about it.

## For contributors: the privacy guard

The repository is public, so nothing personal may enter it. `scripts/privacy-check.sh` and `scripts/privacy-check.ps1` compare the tracked files (and, with `--history`, every commit's diff and message) against a denylist of the maintainer's private identifiers. They report the file and line of a match without printing the matched text. The denylist itself is never committed. It comes from the `PRIVACY_DENYLIST` CI secret, or from the untracked file `.git/info/privacy-denylist` in a local clone. Enable the pre-commit hook with `git config core.hooksPath .githooks`. See [CONTRIBUTING.md](../CONTRIBUTING.md).
