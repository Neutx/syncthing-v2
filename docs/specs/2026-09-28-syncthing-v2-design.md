# SyncThing V2: design specification

- Status: approved for implementation
- Date: 2026-09-28
- Scope: version 1.0.0
- Supersedes: the Windows-only "SyncthingTray" prototype (C# WinForms tray and "Liquid Glass" dashboard)

This is the single source of truth for implementation. Where this spec and the prototype disagree, this spec wins. Where it is silent on an existing behaviour, the prototype's code behaviour (not its old design notes) is the reference. §13 lists that behaviour item by item.

---

## 0. Working name and constants

The product name is a **working name** pending the maintainer's final confirmation (§15, D1). Every user-visible name, slug and identifier comes from one file, `internal/brand/brand.go`, so a rename takes one commit.

| Constant | Value |
|---|---|
| `DisplayName` | `SyncThing V2` |
| `BinaryName` | `stv2` (`stv2.exe` on Windows) |
| `RepoOwner` / `RepoName` | `Neutx` / `syncthing-v2` |
| `BundleID` (macOS) / reverse-DNS prefix | `io.github.neutx.syncthingv2` |
| `AppDirName` (per-user data dir name) | `SyncThingV2` (Windows, macOS), `syncthing-v2` (Linux) |
| `MutexName` (Windows) | `Local\SyncThingV2.Tray` (the prototype's mutex name must stay different) |
| `Version` | injected at link time: `-X <module>/internal/brand.Version=<x.y.z>`; `0.0.0-dev` otherwise |
| `MinSyncthing` | `v1.27.0` |
| `BootstrapSyncthing` | `v2.1.5` (bumped through `scripts/update-syncthing-pin.sh`) |
| Module path | `github.com/Neutx/syncthing-v2` |

**Disclaimer text** (`brand.Disclaimer`). It appears verbatim in the README footer, in the About dialog/menu, and in `stv2 version`:

> SyncThing V2 is an independent open-source project. It is not affiliated with, endorsed by, or sponsored by the Syncthing Foundation or Tailscale Inc. "Syncthing" is a trademark of the Syncthing Foundation. "Tailscale" is a trademark of Tailscale Inc. SyncThing V2 does not use the Syncthing logo.

---

## 1. Goals and non-goals

### Goals

1. A user who has Tailscale on two computers can install SyncThing V2 on both and sync a folder between them with **no manual device-ID copying and no port or IP configuration**.
2. One-step install on each OS:
   - **Windows:** a self-installing `.exe` from GitHub Releases, or a PowerShell one-liner.
   - **macOS:** `.dmg`, or a `curl | sh` one-liner.
   - **Linux:** `.deb` or `.tar.gz`, or a `curl | sh` one-liner.
3. **Keep every prototype feature** (§13): the tray icon, tooltip, menu, notifications, live status engine and the Liquid Glass dashboard. On Windows the glass is at full fidelity; the other platforms use a native or flat material.
4. **Syncthing is ensured.** SyncThing V2 adopts an existing Syncthing untouched. If there is none, it downloads a pinned, checksum-verified upstream release into a user-writable directory and keeps upstream's auto-upgrade on.
5. **Secure by default:**
   - no new network listeners
   - Syncthing's GUI and API stay on loopback
   - consent on both machines before pairing
   - no secrets in the repo, logs or diagnostics
6. **Reproducible, attested releases for all three OSes** from GitHub Actions. The Windows build also works locally with only the Go toolchain installed.
7. **Complete public docs:** README, per-OS install guides, Tailscale setup, pairing, architecture, security, troubleshooting, building, releasing, CONTRIBUTING, SECURITY, LICENSE, CHANGELOG and THIRD_PARTY_NOTICES.

### Non-goals for 1.0

- Replacing Syncthing's web UI. Advanced configuration still happens there, through "Open Web UI".
- Bundling or installing Tailscale silently. Tailscale needs admin rights and its own sign-in; SyncThing V2 detects it and links to it (plus an optional `winget` install on Windows with consent).
- Automatic address healing when a node's tailnet IP changes. Syncthing's `dynamic` address, added after the first connection, covers most cases.
- These are also out of scope:
  - Tailscale LocalAPI
  - MagicDNS addresses in Syncthing config
  - QR or code pairing
  - `.rpm`
  - AppImage
  - a Windows arm64 native build (x64 runs under emulation)
  - Authenticode or Apple notarization (the pipeline has pre-wired conditional steps, but 1.0 ships unsigned)
  - auto-downloading SyncThing V2 updates (1.0 only *notifies*)
  - mobile platforms

---

## 2. Architecture

### 2.1 Stack

- **Language:** Go 1.27.x. `go.mod` declares `go 1.27` and `toolchain go1.27.1`, or the current 1.27 patch release at implementation time.
- **Binaries:** one per OS/arch, with no runtime to install.
  - Windows and Linux build with `CGO_ENABLED=0`.
  - macOS builds with cgo, which Cocoa needs.
- **Dashboard:** vanilla HTML, CSS and JS. There is no npm and no bundler. The files are embedded with `go:embed`, served by a loopback HTTP server inside the tray process, and shown by a small host process:

| OS | Dashboard host |
|---|---|
| Windows | WebView2 in a borderless, squircle-clipped Win32 popup. The backdrop is the Go port of the prototype's glass pipeline. |
| macOS | A WKWebView in a non-activating `NSPanel` over an `NSVisualEffectView`, written in about 150 lines of ObjC in `host_darwin.m`. |
| Linux | The default browser, with a flat translucent-look material. |

**Why not the alternatives:** keeping C# gives no macOS or Linux path (and there is no SDK); Electron is about 100 MB; Tauri needs a Rust and MSVC toolchain; Avalonia means two UI hosts and roughly 80 MB builds.

### 2.2 Third-party modules

Each module is pinned to its latest release tag at implementation time and recorded in `go.sum` and THIRD_PARTY_NOTICES.

| Module | Use | cgo |
|---|---|---|
| `golang.org/x/sys` | Win32 (tray, registry, windows, GDI capture, mutex), unix flock | no |
| `golang.org/x/image` (`vector`) | icon rasteriser | no |
| `fyne.io/systray` | tray on macOS and Linux only | macOS: yes; Linux: no |
| `github.com/godbus/dbus/v5` | Linux notifications and StatusNotifierWatcher detection (already a systray dependency) | no |
| `github.com/wailsapp/go-webview2` (`pkg/edge`) | WebView2 controller embedded in our own Win32 window | no |
| `github.com/go-ole/go-ole` | `IShellLinkW` for the Start Menu shortcut (Windows) | no |

Build-time tools only, run with `go run <module>@<pinned version>`:

- `github.com/tc-hib/go-winres` (icon, manifest and version resources)
- `github.com/goreleaser/nfpm/v2/cmd/nfpm` (the `.deb`)
- `honnef.co/go/tools/cmd/staticcheck`
- `golang.org/x/vuln/cmd/govulncheck`

Everything else is standard library: `net/http`, `net/netip`, `crypto/tls`, `crypto/sha256`, `encoding/xml`, `encoding/json`, `archive/zip`, `archive/tar`, `compress/gzip`, `os/exec`, `embed`, `image/png`.

**Dependency anchor:** `internal/deps/deps_{windows,darwin,linux}.go` blank-imports every runtime module under the matching build tags. This means `go.mod` and `go.sum` are written once, in the foundation task, and stay tidy while the other packages are being built in parallel.

### 2.3 Process model

```
                 ┌──────────────── stv2 (tray process; main thread = tray loop) ─────────────────┐
 OS autostart ──►│ app.App                                                                        │
 (--background)  │  ├─ status.Engine  ── poll 2 s + /rest/events long-poll ──► Syncthing REST     │
                 │  │     └─ publishes model.Snapshot (immutable) on a channel                    │
                 │  ├─ tray.Tray (Win32 native on Windows; fyne.io/systray on macOS/Linux)        │
                 │  ├─ notify.Notifier (balloons / D-Bus / osascript argv)                        │
                 │  ├─ pairing.Watcher (PendingDevicesChanged, PendingFoldersChanged)             │
                 │  ├─ ui.Server 127.0.0.1:0  (SSE /api/state, POST /api/*; token+cookie auth)    │
                 │  ├─ glass.Render (Windows only: capture → refracted backdrop PNG)              │
                 │  └─ update.Checker (daily, read-only GitHub Releases query)                    │
                 └───────────────▲────────────────────────────────────────────────────────────────┘
                                 │ stdin line protocol: "auth <token>", "show x y w h", "hide", "quit"
                 ┌───────────────┴──── stv2 ui-host (child; prewarmed on Windows/macOS) ──────────┐
                 │ Windows: WebView2 popup     macOS: NSPanel + vibrancy + WKWebView               │
                 └─────────────────────────────────────────────────────────────────────────────────┘
                 Linux: no ui-host; a 0600 launch file is opened with xdg-open (§6.4).

 Syncthing: its own process, started by OS autostart (HKCU Run / LaunchAgent / systemd --user
 or XDG autostart). It is never a child of the tray, so a tray crash or exit never stops sync.
 Tailscale: read-only CLI calls (`tailscale status --json`).
```

- **State ownership:** `status.Engine` owns all mutable status state in one goroutine. Every other part only receives immutable `model.Snapshot` values.
- **UI thread:**
  - On Windows, tray callbacks run on the tray's locked OS thread.
  - On macOS and Linux, fyne systray calls back on its own thread, and the app marshals work through channels.

### 2.4 Repository layout (final)

```
.github/workflows/ci.yml            .github/workflows/release.yml
.github/ISSUE_TEMPLATE/{bug_report.yml,feature_request.yml,config.yml}
.github/PULL_REQUEST_TEMPLATE.md    .github/dependabot.yml
.githooks/pre-commit
cmd/stv2/main.go
internal/brand/        names, version, disclaimer, per-OS strings
internal/model/        shared data types (Snapshot, State, Peer, Folder, ActivityItem, Candidate, Pending*)
internal/deps/         dependency anchor (blank imports, build-tagged)
internal/applog/       rolling log (1 MiB × 2) with redaction
internal/prefs/        prefs.json (atomic write)
internal/single/       single-instance + instance.json control file
internal/osutil/       app dirs, OpenURL/OpenFolder, hidden/detached exec, home dir
internal/stclient/     REST client, typed errors, config discovery, config.xml read, security audit
internal/status/       engine, state machine, formatters, transport classifier, activity map, notices, diagnostics
internal/stinstall/    detect/adopt, download+verify+extract, generate, start, profile, version check, pins.go
internal/autostart/    tray + Syncthing autostart per OS
internal/tailnet/      tailscale CLI locate, status parse, filters, prefixes
internal/deviceid/     cert DER → device ID, Short()
internal/pairing/      probe, Decide(), Pair/Accept/Decline, folder share/accept, watcher
internal/icon/         state ring rasteriser → RGBA/PNG/ICO; cmd/genicons
internal/tray/         tray_windows.go (native Win32), tray_other.go (fyne), menu model
internal/notify/       notify_windows.go (NIF_INFO via tray), notify_darwin.go, notify_linux.go
internal/ui/           server, auth, SSE, API; web/ (index.html, app.css, tokens.css, app.js, anim.js)
internal/glass/        Go port of Glass.cs + capture_windows.go
internal/uihost/       host_windows.go, host_darwin.go + host_darwin.m, host_linux.go, placement_windows.go
internal/picker/       native folder picker (SHBrowseForFolderW / osascript / zenity|kdialog|none)
internal/install/      self-install/uninstall/upgrade per OS, Start Menu lnk, Uninstall key, legacy migration, firewall
internal/doctor/       checks + codes, text/JSON output
internal/update/       release check
internal/app/          wiring: engine → tray/notify/ui; command dispatch; setup flow
e2e/                   two-instance pairing test (build tag e2e)
packaging/windows/     winres.json, app.manifest
packaging/macos/       Info.plist, make-app.sh, make-dmg.sh
packaging/linux/       nfpm.yaml, stv2.desktop
scripts/               install.ps1, install.sh, build.ps1, build.sh, update-syncthing-pin.sh,
                       privacy-check.sh, privacy-check.ps1, screenshots.ps1
build/                 syncthing-release-key.asc
assets/icons/          generated app icon PNGs (16..1024) + icon.ico
assets/screenshots/    demo-mode captures only
docs/                  install-windows.md, install-macos.md, install-linux.md, tailscale-setup.md, pairing.md,
                       architecture.md, security.md, troubleshooting.md, building.md, releasing.md,
                       uninstall.md, migrating-from-manual-setup.md, design.md, specs/
README.md CONTRIBUTING.md SECURITY.md LICENSE CHANGELOG.md THIRD_PARTY_NOTICES.md
.gitignore .gitattributes .editorconfig go.mod go.sum
```

- **Tests:** unit tests sit next to each package as `*_test.go`. Fixtures in `testdata/` are **synthetic only**.
- **Generated files are committed:**
  - `internal/stinstall/pins.go`
  - `assets/icons/*`
  - `packaging/windows/*.syso` is **not** committed; it is generated at build time.

---

## 3. Components and interfaces

The signatures below are contracts between parallel implementation tasks. Names are exact.

### 3.1 `internal/model`

```go
type State int
const (StateDown State = iota; StateUnauthorized; StateError; StatePaused; StateSyncing; StateScanning; StateNoPeer; StateInSync)

type Transport string // "Tailscale" | "relay" | "local network" | "direct"

type Peer struct { ID, Name, Addr string; Transport Transport; Connected bool }
type Folder struct { ID, Label, Path string; Paused bool; State string; Errors int
    GlobalBytes, NeedBytes, InSyncBytes, GlobalFiles, LocalFiles, NeedItems int64 }
type ActivityItem struct { At time.Time; Text string; Tint string } // tint: green|red|blue|amber|grey
type Startup struct { Syncthing, Tray bool; SyncthingManagedByUs bool }

type Snapshot struct {
    State State; Label, Headline, Subline string
    Detail string                      // error detail for Down/Unauthorized/Error
    Peers []Peer                       // configured remote devices; Connected flag set
    Folders []Folder; Pct int; NeedBytes, NeedItems int64
    InRate, OutRate float64; Moving bool; ETA string
    PeerLine, FolderLine, FilesLine, SizeLine, SpeedLine string
    Activity []ActivityItem            // newest first, max 40
    Startup Startup
    GUIURL string                      // for "Open Web UI"; never contains the API key
    UpdateAvailable string             // "" or "vX.Y.Z"
    Pending []PendingDevice; PendingFolders []PendingFolder
    Notices []string                   // one-time prompts: "gui-exposed", "legacy-tray", "tailscale-missing"
    At time.Time
}

type Candidate struct { NodeName, DNSName, OS, LoginName string; IP netip.Addr
    SameOwner bool; DeviceID string; Status string } // status: "ready"|"paired"|"no-syncthing"|"blocked"|"self"
type PendingDevice struct { DeviceID, Name string; Addr netip.AddrPort; Verified bool; Node *Candidate; Reason string }
type PendingFolder struct { FolderID, Label, FromDevice string }
```

### 3.2 `internal/stclient`

```go
type Endpoint struct { BaseURL *url.URL; APIKey string; CAPool *x509.CertPool } // CAPool set when GUI TLS is on
type ErrKind int; const (ErrUnreachable ErrKind = iota; ErrUnauthorized; ErrBadResponse)
type Error struct { Kind ErrKind; Status int; Err error }

func Discover(ctx context.Context, syncthingBin string) (Endpoint, string /*configPath*/, error)
func ReadConfig(configPath string) (Endpoint, GUIConfig, error)
type GUIConfig struct { Address string; TLS, InsecureAdminAccess, HasUser bool; Port int }
func Audit(g GUIConfig) []Finding            // Finding{Code:"SEC001", Severity:"high", Text}
type Client struct{ /* one http.Client */ }
func New(ep Endpoint) *Client
func (c *Client) Get(ctx context.Context, path string, out any) error        // 5 s timeout
func (c *Client) Events(ctx context.Context, since int, timeout time.Duration) ([]json.RawMessage, int, error) // 60 s
func (c *Client) Send(ctx context.Context, method, path string, body any) error // POST/PUT/PATCH/DELETE, 6 s
```

- **Config path discovery,** in this order:
  1. `STHOMEDIR`, or `STCONFDIR`, plus `/config.xml`
  2. `<bin> paths`, reading the `Configuration file:` line (v2)
  3. `<bin> --paths` (v1)
  4. the known default for each OS:
     - Windows: `%LOCALAPPDATA%\Syncthing\config.xml`
     - macOS: `~/Library/Application Support/Syncthing/config.xml`
     - Linux: `$XDG_STATE_HOME/syncthing`, `~/.local/state/syncthing`, then `~/.config/syncthing`
- **GUI address normalisation** (F2): the wildcards `0.0.0.0`, `::`, `*` and empty become `127.0.0.1:<port>`. The default is `127.0.0.1:8384`.
- **HTTPS:** used when `tls="true"`. The client trusts only `https-cert.pem` from the config dir and connects to the loopback host only.
- **Errors:**
  - A 401 or 403 becomes `ErrUnauthorized`.
  - A connection refused or timeout becomes `ErrUnreachable`.
  - A JSON decode failure or unexpected status becomes `ErrBadResponse`.

### 3.3 `internal/status`

```go
type Engine struct{ /* owns all mutable state */ }
func NewEngine(c *stclient.Client, st Startup func() model.Startup) *Engine
func (e *Engine) Run(ctx context.Context) <-chan model.Snapshot
func (e *Engine) Refresh()                                   // non-blocking nudge
func (e *Engine) Note(text, tint string)                     // command activity notes (F28)
func Classify(addr, connType string) model.Transport
func Describe(ev json.RawMessage) (model.ActivityItem, bool) // §2.4 of the inventory
func NoticeFor(prev, next model.State, s model.Snapshot) (title, body string, ok bool) // F23
func Diagnostics(s model.Snapshot, extra map[string]string) string // redacted (IDs → 7 chars, no paths/IPs outside 100.64/10)
func Rate(bps float64) string; func Size(b int64) string; func Grp(n int64) string; func Label(s model.State) string
```

**Polling:**

- **Startup:** read `/rest/system/status` (for `myID`), `/rest/config/devices`, `/rest/config/folders`, and `/rest/db/status?folder=` for each folder.
- **Every 2 s:** `/rest/system/connections` (rates, peers), plus the pending lists every 30 s.
- **Config refresh:** on a `ConfigSaved` event.
- **Folder numbers:**
  - from `FolderSummary` events
  - on `StateChanged`, `/rest/db/status` is fetched for that folder only
  - a full reconcile every 30 s and after every command
- The event long-poll keeps F6 exactly: seed with `limit=1`, then `since=N&timeout=50`, and back off 3 s on error.
- **Commands** stay the same: rescan, restart, pause and resume. Pause and resume send `PATCH /rest/config/folders/{id}` with `{"paused":bool}` for every folder.

**State machine** (this fixes latent bug 1). `peersUp` counts connected remote devices; `remotes` counts configured remote devices. The first matching rule wins:

1. The client error is `ErrUnreachable` → `Down`. Detail: "Syncthing is not responding on 127.0.0.1:<port>".
2. The client error is `ErrUnauthorized` → `Unauthorized`. Detail: "Syncthing rejected the API key — run `stv2 doctor`".
3. The client error is `ErrBadResponse` → `Error`. Detail: "Unexpected response from Syncthing".
4. There are no folders → `InSync` with FolderLine "No folders configured". If `remotes == 0`, the subline adds "Pair a device to start".
5. Every folder is paused → `Paused`.
6. Any folder has errors or pull errors → `Error`.
7. `peersUp == 0` → `NoPeer`. If `NeedBytes > 0`, the headline is "Disconnected" and the FolderLine is "NN% - X left".
8. A folder is syncing or sync-preparing, or `NeedBytes > 0` → `Syncing`.
9. A folder is scanning or scan-waiting → `Scanning`.
10. Otherwise → `InSync`.

**Percent** (F13): `floor(100·(global−need)/global)`, capped at 99 while `need > 0`, and 100 when there is no data.

**Rates:** computed only over intervals longer than 0.5 s, and a counter drop is never counted (F10).

**Transport** (fixes bug 4). `Classify` checks, in this order:

1. The type contains "relay" → `relay`.
2. The host IP (parsed with `netip.ParseAddrPort`; IPv6 zones are allowed) is in `100.64.0.0/10` or `fd7a:115c:a1e0::/48` → `Tailscale`.
3. It is in `10/8`, `172.16/12`, `192.168/16`, `169.254/16`, `fc00::/7` or `fe80::/10` → `local network`.
4. Otherwise → `direct`.

**Text** (F15, F16, F18): every string and formatter is ported verbatim. The exceptions are the generalised copy below and the per-OS wording in `brand`.

| Prototype text | V2 text |
|---|---|
| "Both laptops hold the same files." | "All devices hold the same files." |
| "The other laptop is not reachable right now." | "No paired device is reachable right now." |
| "Waiting for the other laptop" | "Waiting for a device" |

**PeerLine:** lists every connected peer as `Name (Transport)`, comma-separated.

### 3.4 `internal/stinstall`

```go
type Install struct { Bin, ConfigPath string; Managed bool; Version string; Running bool; AutostartExists bool }
func Detect(ctx context.Context) (Install, error)      // §5.1
func Download(ctx context.Context, dir string, progress func(done, total int64)) (string, error) // pinned + verified
func Generate(ctx context.Context, bin, home string) error   // `generate` with port probing (no fixed ports)
func Start(bin string, extraArgs ...string) error            // detached; never a child
func ApplyProfile(ctx context.Context, c *stclient.Client, p Profile) error // Profile: Tailnet | Hybrid
func CheckVersion(v string) (ok bool, msg string)            // >= brand.MinSyncthing
```

### 3.5 `internal/autostart`

```go
type Target int; const (Tray Target = iota; Syncthing)
func Enabled(t Target) (bool, error)
func Set(t Target, on bool, bin string, args []string) error
func Existing(t Target) (desc string, ok bool) // detects foreign entries (Startup .lnk, other LaunchAgents, distro units)
```

### 3.6 `internal/tailnet`

```go
type Source interface { Status(ctx context.Context) (Status, error) } // production: CLI; tests: fake
type CLI struct{ Path string }                                      // Locate(): PATH, then per-OS known paths
func Locate() (string, error)
type Node struct { ID, HostName, DNSName, OS, LoginName string; IPs []netip.Addr; Online, Sharee bool; Tags []string; UserID int64 }
type Status struct { BackendState string; Self Node; Peers []Node }
func (s Status) FindByIP(ip netip.Addr) (Node, bool)
func (s Status) Candidates() []Node // Online, OS ∈ {windows, macOS, linux}, !Sharee, no Tags; same-owner first
var Prefixes = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")}
```

**Known CLI paths:**

| OS | Paths |
|---|---|
| Windows | `%ProgramFiles%\Tailscale\tailscale.exe` |
| macOS | `/Applications/Tailscale.app/Contents/MacOS/Tailscale`, `/opt/homebrew/bin/tailscale`, `/usr/local/bin/tailscale` |
| Linux | `/usr/bin/tailscale`, `/usr/local/bin/tailscale`, `/snap/bin/tailscale` |

- `DNSName` has its trailing dot trimmed.
- `LoginName` comes from the status JSON's `User[UserID].LoginName`.
- The CLI runs with a hidden window on Windows and a 5 s timeout.

### 3.7 `internal/deviceid` and `internal/pairing`

```go
func deviceid.FromCert(der []byte) string // SHA-256 → base32 (no pad, 52) → 4×(13 + Luhn-32 char) → groups of 7 with '-'
func deviceid.Short(id string) string     // first 7 chars

func pairing.Probe(ctx context.Context, ap netip.AddrPort) (string, error) // ErrNoSyncthing | ErrRefused | ErrTimeout
type Policy struct { Prefixes []netip.Prefix }                             // production: tailnet.Prefixes
type Decision struct { Action string /* "prompt"|"ignore" */; Reason string; Node *tailnet.Node }
func (p Policy) Decide(pd model.PendingDevice, ts tailnet.Status, probedID string, probeErr error) Decision
type Service struct { C *stclient.Client; TS tailnet.Source; Policy Policy; Probe func(context.Context, netip.AddrPort) (string, error) }
func (s *Service) Discover(ctx context.Context) ([]model.Candidate, error)
func (s *Service) Pair(ctx context.Context, c model.Candidate) error
func (s *Service) Accept(ctx context.Context, pd model.PendingDevice) error
func (s *Service) Decline(ctx context.Context, deviceID string) error
func (s *Service) Widen(ctx context.Context, deviceID string) error           // add "dynamic" after first connect
func (s *Service) ShareFolder(ctx context.Context, folderID, label, path string, deviceIDs []string) error
func (s *Service) AcceptFolder(ctx context.Context, pf model.PendingFolder, path string) error
func (s *Service) DeclineFolder(ctx context.Context, pf model.PendingFolder) error
func SafeFolderDir(base, label, id string) (string, error) // sanitised, unique path under base
type Watcher struct{ /* dedupes by device ID; emits model.PendingDevice with Verified set */ }
```

### 3.8 `internal/tray`, `internal/notify`, `internal/icon`

```go
type MenuItem struct { ID, Text string; Enabled, Checked, Visible bool; Children []MenuItem }
type Tray interface {
    Run(onReady func(), onClick func(), onMenu func(id string)) // blocks; owns the OS thread
    SetIcon(s model.State, pct int)                             // pct quantised to 5% steps off Windows
    SetTooltip(text string)                                     // Windows: ≤127 chars native
    SetMenu(items []MenuItem)
    Balloon(title, body string) bool                            // Windows only; false elsewhere
    Quit()
}
func tray.New() Tray
func tray.BuildMenu(s model.Snapshot) []MenuItem                // pure; tested
func tray.Tooltip(s model.Snapshot) string                      // pure; F20 rules; ≤127 chars

func notify.Show(title, body string, onClick func())            // Windows: via Tray.Balloon; mac: osascript argv; Linux: D-Bus
func icon.Ring(s model.State, pct int, size int) *image.RGBA    // F19
func icon.PNG(img *image.RGBA) []byte; func icon.ICO(imgs ...*image.RGBA) []byte
```

### 3.9 `internal/ui`, `internal/uihost`, `internal/glass`, `internal/picker`

```go
type Backend interface {
    Snapshots() (<-chan model.Snapshot, func())       // subscribe/unsubscribe
    Action(ctx context.Context, name string, args json.RawMessage) (any, error)
    Backdrop() []byte                                  // PNG or nil
}
type Server struct{ /* listener on 127.0.0.1:0 */ }
func ui.NewServer(b Backend) (*Server, error)
func (s *Server) URL() string; func (s *Server) NewLaunchToken() string // single-use, 30 s TTL
func (s *Server) ControlToken() string                                  // for instance.json /api/show
func ui.Demo() Backend                                                  // synthetic data, used by `stv2 demo`

type Host interface { Show(r image.Rectangle); Hide(); Close() error }
func uihost.Start(serverURL, launchToken string) (Host, error)          // spawns `stv2 ui-host`; Linux: launch file
func uihost.RunChild() error                                            // `stv2 ui-host` entry
func uihost.Place(sizeDIP image.Point) (rect image.Rectangle, scale float64) // Windows: monitor under cursor, taskbar edge

func glass.Capture(r image.Rectangle) (*image.RGBA, error)             // Windows BitBlt; others: ErrUnsupported
func glass.Render(src *image.RGBA, scale float64) *image.RGBA           // D3 pipeline, code constants
func picker.Folder(title, initial string) (string, error)              // "" if cancelled or unavailable
```

**ui API.** All routes pass the Host and Origin checks (§8.3). All routes except `/login` and the bearer control routes need the session cookie. Every POST, the control routes included, also needs the header `X-STV2: 1`.

| Route | Purpose |
|---|---|
| `GET /` | static app |
| `POST /login` | exchange a launch token for a cookie |
| `GET /login?token=&next=` | the same exchange for printed links (`stv2 demo`, `/api/launch`); single-use, 30 s |
| `GET /api/state` | SSE, `event: snapshot` |
| `GET /api/backdrop.png` | glass backdrop |
| `POST /api/action` | body `{"name":...,"args":...}` |
| `GET /api/info` | name, version, commit, disclaimer, per-OS wording for the About view |
| `POST /api/show` | bearer control token; used by a second instance |
| `POST /api/quit` | bearer control token; used by the installer |
| `POST /api/launch` | bearer control token; body `{"next"?}`, returns a fresh single-use login URL |

**Actions:**

- `rescan`, `pause`, `resume`, `restart`, `start-syncthing`
- `open-folder {id?}`, `open-webui`, `copy-diagnostics`
- `set-autostart {target,on}`
- `discover`, `pair {deviceID}`
- `accept-device {deviceID}`, `decline-device {deviceID}`
- `share-folder {folderID?, path?, deviceIDs}`, `pick-folder`
- `accept-folder {folderID, fromDevice, path?}`, `decline-folder {...}`
- `fix-gui-exposure`, `dismiss-notice {id, forever?}` (`id` is a `Snapshot.Notices` entry, or `update` for the update banner)
- `migrate-legacy {yes}`
- `install-tailscale`, `open-update`
- `get-settings`, `set-pref {key, on}`, `set-profile {profile}`, `open-logs` (Settings view)
- `open-docs {page}` (a fixed list of documentation pages, see `ui.DocsURL`)
- `hide`

`ui.Actions()` is the authoritative list. The dashboard backend (`internal/app`) and the demo backend each have a test that fails when a listed action is not handled. An action error may carry a documentation page (`ui.WithDocs`); the page then shows a help link next to the message.

### 3.10 CLI (`cmd/stv2`)

| Command | Behaviour |
|---|---|
| *(none)* | Run the tray; if already running, POST `/api/show` to the running instance and exit 0. On Windows, if the exe is not the installed copy, ask "Install SyncThing V2 for <user>?" (MB_YESNO) → `install`. |
| `--background` | Autostart mode: tray only, no dashboard. |
| `--setup` | Run the tray and open the dashboard's welcome view. |
| `install [--yes] [--no-start]` | §6.1. |
| `uninstall [--yes] [--remove-syncthing]` | §6.1; never deletes Syncthing config or synced data. |
| `pair --list [--json]` | Print candidates with short IDs and status. |
| `doctor [--json]` | §3.11; exit code 0 if no `high` findings, 1 otherwise. |
| `firewall allow` | Windows only; re-launches elevated (UAC) and adds the scoped rule (§5.5). |
| `profile tailnet\|hybrid` | Apply a transport profile to the current Syncthing after a confirmation prompt. |
| `demo [--port N]` | Serve the dashboard with synthetic data on 127.0.0.1 (screenshots, UI work). |
| `ui-host` | Internal (child process). |
| `version` | Version, commit, Go version, disclaimer. |

### 3.11 Doctor codes

Each code maps to a section of `docs/troubleshooting.md`.

| Code | Severity | Check |
|---|---|---|
| TS001 | high | Tailscale CLI not found |
| TS002 | high | `BackendState != "Running"` |
| ST001 | high | Syncthing binary not found |
| ST002 | high | Syncthing not responding (Down) |
| ST003 | high | API key rejected (Unauthorized) |
| ST004 | warn | Syncthing version below `MinSyncthing` |
| ST005 | info | Syncthing sync listener not on the default port 22000 (pairing probes need 22000; see docs) |
| SEC001 | high | GUI reachable beyond loopback without a password, or `insecureAdminAccess=true` |
| NET001 | warn | For each online same-owner peer: Syncthing not reachable on `<100.x>:22000` (ACL, firewall, or not installed) |
| FW001 | info | Windows: the "SyncThing V2 - Syncthing" firewall rule is absent |
| UI001 | warn | Windows: WebView2 runtime missing (the dashboard uses the browser fallback) |
| UI002 | warn | Linux: no StatusNotifierWatcher on the session bus |
| UPD001 | info | A newer SyncThing V2 release exists |

---

## 4. Tailscale pairing protocol and threat model

### 4.1 Principles

- **No new listeners, ports or secrets on the wire.** Identity comes from three sources:
  1. **Tailscale:** WireGuard binds a 100.x source address to a node and its owner.
  2. **Syncthing:** mutual TLS binds a device ID to its key.
  3. **The user:** an explicit click on each machine.
- **Consent on both sides.** Nothing is ever accepted without a click on *that* machine: no device and no folder.
- **Idempotent writes.** Devices use `PUT /rest/config/devices/{id}` (an upsert). Folder shares GET the folder, merge into `devices` by ID and `PUT` it back. They never replace the list.

### 4.2 Discovery (device A)

Discovery runs **only** at tray startup (once), when the Pair view opens, and when the user clicks "Refresh". There is no periodic background sweep.

1. **Check Tailscale.** `tailnet.Source.Status()` must return `BackendState == "Running"`. Otherwise:
   - The Pair view shows "Tailscale is not connected. Open Tailscale and sign in", with a link to the docs.
   - If the CLI is missing, it shows the install card. On Windows, that card includes an "Install with winget" button (`winget install -e --id Tailscale.Tailscale`, run visibly and only on click).
2. **List candidates** with `Status.Candidates()`, which keeps only online peers, running Windows, macOS or Linux, that are not shared-in nodes and have no tags.
   - Same-owner nodes are listed under "Your devices".
   - Other owners are listed under "Other people's devices", with a warning badge.
3. **Probe each candidate:** `Probe(ctx, <IPv4>:22000)`. At most 4 run in parallel, with a 3 s timeout each.
   - The TLS config uses `MinVersion: tls.VersionTLS13`, `NextProtos: ["bep/1.0"]`, `InsecureSkipVerify: true`, and a `VerifyPeerCertificate` that captures `rawCerts[0]` and **returns `errProbeDone`**.
   - Returning that error aborts the handshake before the client sends any certificate, so the target's Syncthing never records a pending device. An e2e test asserts this (§10.3).
   - `GetClientCertificate` is never set.
   - The result sets the candidate's status:

| Result | Status | Shown as |
|---|---|---|
| A device ID | `ready` | ready to pair |
| The ID is already configured | `paired` | already paired |
| The ID equals our own ID | `self` | not shown |
| Connection refused | `blocked` | "Syncthing not reachable — is SyncThing V2 running there? Check ACLs/firewall" (docs link) |
| Timeout or TLS failure | `no-syncthing` | "Install SyncThing V2 on this device (or it uses a non-default port)" |

### 4.3 Pairing A → B

1. The user on A clicks **Pair** on candidate B. A runs `PUT /rest/config/devices/{B}` with:
   - `name: B.HostName`
   - `addresses: ["tcp://<B-ip4>:22000", "quic://<B-ip4>:22000"]`
   - `autoAcceptFolders: false`
   - `introducer: false`
   - `compression: "metadata"`
2. A's Syncthing dials B.
   - **If B has already added A** (the user clicked Pair on both machines), the connection comes up and pairing is done with no prompts.
   - Otherwise B's Syncthing rejects A and records it under `/rest/cluster/pending/devices` (event `PendingDevicesChanged`).
3. **B's `pairing.Watcher`** runs on the event, with a 30 s poll as fallback. For each pending device not already prompted (deduped by device ID; dismissed IDs are kept in prefs), it evaluates `Policy.Decide`:
   - a. The pending `address` IP must be inside `Policy.Prefixes`. Otherwise **ignore**, with reason "not from tailnet". It is left to Syncthing's web UI and logged once.
   - b. A fresh `tailscale status` must contain a node with that IP. Otherwise **ignore** ("unknown tailnet node").
   - c. `Probe(<ip>:22000)` must equal the pending device ID. Otherwise **ignore** ("identity mismatch"). This is the defence against a subnet router or exit node rewriting source addresses (SNAT).
   - d. If all three pass → **prompt**, with `Verified = true`.
     - The prompt text is "**<HostName>** wants to sync with this computer · <OS> · ID `ABCDEFG`".
     - If the node's `UserID` differs from Self, the prompt adds "Owned by **<LoginName>** — only accept if you know this person" and shows a red badge.
     - The prompt appears as a notification (a click opens the dashboard Pair view) and as a dashboard banner.
4. **Accept on B:** `PUT /rest/config/devices/{A}` with A's tailnet addresses (`tcp://` and `quic://` on `<A-ip4>:22000`) and `autoAcceptFolders: false`.
   **Decline on B:** `DELETE /rest/cluster/pending/devices?device={A}` and add A to `dismissedDevices`.
5. **Widen.** On the first `DeviceConnected` for a device that V2 paired, `Widen` PATCHes its addresses to `[tcp…, quic…, "dynamic"]`, so LAN, local discovery and (in the hybrid profile) global fallbacks also work. It records `widened[id]=true` in prefs so this runs only once.
6. **Races.** If both users click Pair at the same moment, both PUTs are upserts; the pending entries clear when the connection comes up. The Watcher drops a pending prompt whose device ID has since appeared in `/rest/config/devices`.

**One-sided inbound.** Each side configures the other's tailnet address. Syncthing needs only **one** successful dial in either direction, so pairing works when only one machine accepts inbound 22000.

### 4.4 Folder sharing

- **On A:** the dashboard's "Share a folder" view lists existing folders plus "New folder…".
  - "New folder…" calls `picker.Folder` (default `~/Sync`, created if missing), with label = the base name and ID = a random `xxxxx-xxxxx` (lowercase letters and digits, from `crypto/rand`).
  - `ShareFolder` merges the selected device IDs into the folder.
  - **First-run shortcut:** after the first successful pair, the Pair view goes straight to this step.
- **On B:** a `PendingFoldersChanged` event from a **configured** device raises the prompt "<Device> shares "<label>"", with the buttons **Accept** (into `SafeFolderDir(~/Sync, label, id)`), **Choose location…** and **Decline**.
  - A pending folder from an unconfigured device is ignored.
  - Accept sends `PUT /rest/config/folders/{id}` with the path, `devices` = [self, A] merged, and `type: sendreceive`.
- **`SafeFolderDir` sanitises the label:**
  - Keep only `[A-Za-z0-9 ._-]`, collapse runs, and trim leading dots and spaces.
  - Cap it at 64 characters.
  - Reject Windows reserved names (CON, PRN, AUX, NUL, COM1–9, LPT1–9).
  - If the result is empty, use the sanitised folder ID.
  - If the path already exists and is non-empty, append ` (2)`, ` (3)` and so on.
  - The result must stay under `base` after `filepath.Clean`, or it is rejected.

### 4.5 Transport profiles

| Profile | Options | Applied when |
|---|---|---|
| `tailnet` (**default for configs SyncThing V2 generates**) | `globalAnnounceEnabled=false`, `relaysEnabled=false`, `natEnabled=false`, `localAnnounceEnabled=true`, `listenAddresses=["default"]` | Automatically, once, right after `generate`. On adopted configs, only through `stv2 profile tailnet` or the dashboard's Settings toggle, with confirmation. |
| `hybrid` (upstream defaults) | Everything above set back to `true` | `stv2 profile hybrid`, or the Settings toggle. |

- **Trade-off:** under `tailnet`, sync needs Tailscale up, or both machines on the same LAN. This is shown next to the toggle and in `docs/tailscale-setup.md`.
- **Adopted configs keep their current settings.**

### 4.6 Threat model

| # | Threat | Control |
|---|---|---|
| T1 | Another tailnet user's node pairs silently | No auto-accept at all. A different owner gets a warned prompt that shows their login and short ID. Sharee and tagged nodes are excluded from discovery. |
| T2 | A spoofed device ID or source address (SNAT through a subnet router or exit node) | Syncthing mutual TLS proves the pending ID. The probe proves the device on that 100.x address holds that ID (4.3c). The tailnet node lookup proves the address belongs to a node. |
| T3 | LAN, relay or internet pending devices | Never handled by SyncThing V2 (4.3a). They are left to Syncthing's web UI. |
| T4 | Probe side-effects (pending-device spam on peers) | The handshake is aborted before a client certificate is sent. Probes run only on explicit user action or once at startup. |
| T5 | Syncthing GUI/API exposed on the network | Generated configs keep upstream's loopback default. Adopted exposed configs trigger a one-time prompt (§5.4) and doctor SEC001. They are never changed silently. |
| T6 | Local web attacks on the loopback UI server (DNS rebinding, CSRF, cross-origin reads) | Host and Origin checks, a SameSite=Strict HttpOnly cookie, a custom header on POSTs, no CORS headers (§8.3). |
| T7 | UI token leaks through the process list | The token is sent on stdin to ui-host. On Linux it sits in a 0600 launch file in a 0700 dir. It is single-use, expires after 30 s, and changes on every launch. |
| T8 | Syncthing API key leaks | It is never sent to JS or the browser, and never logged: `applog` redacts the key's value and any `X-API-Key` header. Diagnostics exclude it. |
| T9 | Path traversal from a remote folder label, or from a download archive | `SafeFolderDir`, plus a zip-slip and tar-slip guard (reject absolute paths, `..`, and symlinks pointing outside). |
| T10 | Notification or command injection through host names or file names | All exec calls use argument lists. The macOS `osascript` script receives text through `argv`. There is no shell anywhere in the app. |
| T11 | Supply chain (a tampered Syncthing download) | SHA-256 pins compiled into the binary. They are generated from upstream `sha256sum.txt.asc`, GPG-verified against `build/syncthing-release-key.asc`. |
| T12 | Supply chain (the release pipeline) | Actions pinned to commit SHA; `permissions: contents: read` by default; build-provenance attestations; SHA256SUMS; human-published draft releases. |
| T13 | Tampered one-liner install | The scripts verify SHA256SUMS before running anything. The scripts themselves are served from the release (versioned); attestation verification is documented. |
| T14 | Privacy leak of maintainer data into the public repo | A private denylist grep (CI secret plus a local, untracked file) and gitleaks, in CI and in a pre-commit hook. Fixtures are synthetic only. |
| T15 | Update-check privacy | One unauthenticated GET per 24 h to `api.github.com`. It can be disabled in Settings and is documented. |
| — | Out of scope | Malware already running as the same user (it can read Syncthing's own config anyway). A compromised Tailscale control plane. |

---

## 5. Syncthing management per OS

### 5.1 Detect and adopt

`stinstall.Detect` uses the first match:

1. The executable path of a running `syncthing` process. Windows uses `QueryFullProcessImageNameW`; macOS and Linux use `/proc` or `ps -axo pid=,comm=`.
2. `syncthing` on PATH.
3. **Known paths:**
   - Windows: `%LOCALAPPDATA%\Programs\Syncthing\syncthing.exe`, scoop `%USERPROFILE%\scoop\shims\syncthing.exe`, and winget `%LOCALAPPDATA%\Microsoft\WinGet\Links\syncthing.exe`
   - macOS: `/opt/homebrew/bin/syncthing`, `/usr/local/bin/syncthing`, `/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing`
   - Linux: `/usr/bin/syncthing`, `~/.local/bin/syncthing`, `/snap/bin/syncthing`
4. **The managed location,** where SyncThing V2 installs it (`Managed = true`):
   - Windows: `%LOCALAPPDATA%\Programs\SyncThingV2\syncthing\syncthing.exe`
   - macOS: `~/Library/Application Support/SyncThingV2/syncthing/syncthing`
   - Linux: `~/.local/share/syncthing-v2/syncthing/syncthing`

**An adopted Syncthing** (found by steps 1–3) is never modified: not its binary, not its config, not its autostart. SyncThing V2 only reads and uses it. The one exception is an action the user confirms (the GUI-exposure fix, a profile change, or pair and share operations).

### 5.2 Bootstrap when Syncthing is missing

1. **Download** `https://github.com/syncthing/syncthing/releases/download/<BootstrapSyncthing>/<asset>`:
   - Windows: `syncthing-windows-amd64-<v>.zip`
   - macOS: `syncthing-macos-universal-<v>.zip`
   - Linux: `syncthing-linux-{amd64,arm64}-<v>.tar.gz`
   - Show progress in the dashboard welcome view (or on stdout for `install --yes`).
2. **Verify** the SHA-256 against `pins.go`. A mismatch aborts the install and deletes the file.
3. **Extract** to the managed dir with the slip guard. Keep upstream's `LICENSE.txt`, `README.txt` and `AUTHORS.txt` next to the binary, which satisfies MPL-2.0.
4. **Generate:** `syncthing generate --home <default home>`. Port probing stays on, so the GUI and listener pick free ports when 8384 or 22000 are taken. The GUI address and API key are then read from `config.xml`; nothing is hardcoded.
5. **Start** Syncthing detached:
   - Windows: `serve --no-console --no-browser`, with `CREATE_NO_WINDOW | DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`.
   - macOS and Linux: `serve --no-browser --no-restart` under launchd or systemd, which restart Syncthing themselves. With no service manager, `serve --no-browser` started with `Setsid`, keeping Syncthing's own monitor process so that it restarts itself after an upgrade or a crash.
6. **Once the API answers**, apply the `tailnet` profile (§4.5) and set up Syncthing autostart (§5.3).
7. **Upgrades:** Syncthing's own auto-upgrade stays on, because the binary is in a user-writable directory. `pins.go` affects only the bootstrap download. `CheckVersion` warns (ST004) below `MinSyncthing`.

### 5.3 Autostart

| OS | Tray entry | Syncthing entry (created only if managed, or if no autostart exists and the user enables the menu toggle) |
|---|---|---|
| Windows | HKCU `...\CurrentVersion\Run` value `SyncThingV2` = `"<install>\stv2.exe" --background` | HKCU Run value `SyncThingV2-Syncthing` = `"<bin>" serve --no-console --no-browser` |
| macOS | `~/Library/LaunchAgents/io.github.neutx.syncthingv2.plist` (RunAtLoad, `--background`) | `~/Library/LaunchAgents/io.github.neutx.syncthingv2.syncthing.plist` (RunAtLoad, KeepAlive, `serve --no-browser --no-restart`, log to `~/Library/Logs/SyncThingV2/syncthing.log`) |
| Linux | `~/.config/autostart/stv2.desktop` (`Exec=stv2 --background`) | For a distro binary with `/usr/lib/systemd/user/syncthing.service`: `systemctl --user enable --now syncthing.service`. For a managed binary: `~/.config/systemd/user/stv2-syncthing.service` (upstream template semantics: `Restart=on-failure`, `SuccessExitStatus=3 4`, `RestartForceExitStatus=3 4`). If `systemctl --user` is unavailable: `~/.config/autostart/stv2-syncthing.desktop` (`serve --no-browser`, without `--no-restart`, because nothing supervises it). |

- **Detecting an existing autostart:** `autostart.Existing` recognises the following, and reports them as "Syncthing starts at login (configured outside SyncThing V2)" with the toggle shown read-only:
  - Windows Startup-folder `.lnk` files whose target is `syncthing.exe`
  - other HKCU Run values that reference `syncthing`
  - `~/Library/LaunchAgents/*syncthing*.plist`
  - enabled user units named `syncthing*.service`
- **When it is safe to write:** SyncThing V2 creates or removes a Syncthing entry only when it owns that entry.

### 5.4 Security audit prompt for adopted configs

If `stclient.Audit` finds SEC001, a one-time notice (with a dashboard banner and a notification) says:

> "Your Syncthing control panel is reachable from other devices without a password. Restrict it to this computer?"

- **Restrict** sends `PATCH /rest/config/gui` with `{"address":"127.0.0.1:<port>","insecureAdminAccess":false}`.
- **Not now** hides the notice until the next launch.
- **Don't ask again** stores the choice in prefs. Doctor SEC001 still reports it.

### 5.5 Firewalls

- **Windows:**
  - Per-user installs have no admin rights. When Syncthing first listens, Windows Defender Firewall may prompt, and allowing it needs admin.
  - `stv2 firewall allow` is offered in the welcome view and in `docs/troubleshooting.md`. It re-launches elevated (`ShellExecuteW` with the "runas" verb) and runs:
    `netsh advfirewall firewall add rule name="SyncThing V2 - Syncthing" dir=in action=allow program="<bin>" protocol=TCP localport=22000 remoteip=100.64.0.0/10,LocalSubnet enable=yes`
  - It adds the same rule for UDP. There is no IPv6 tailnet rule, because the TS IPv4 address is the one configured.
  - Because pairing needs only one-way dial (§4.3), sync usually works even without the rule.
- **macOS:** the application firewall is off by default. If it is on, the user allows the prompt. The docs note that the prompt returns after a Syncthing self-upgrade.
- **Linux:** the docs give `ufw allow in on tailscale0 to any port 22000`.

---

## 6. SyncThing V2 installation per OS

### 6.1 Windows (self-installing exe; no Inno or NSIS)

- **Release asset:** `SyncThingV2-Setup-<v>-windows-x64.exe`. The same binary is installed as `stv2.exe`.
- **`stv2 install`** (after a MessageBox confirmation, or `--yes`):
  1. If a copy is already installed and running, POST `/api/quit` (control token from `instance.json`) and wait up to 5 s.
  2. Copy itself to `%LOCALAPPDATA%\Programs\SyncThingV2\stv2.exe`. On upgrade, the old exe is renamed to `stv2.exe.old` first; it is deleted on the next start.
  3. Write `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\SyncThingV2` with these values:
     - `DisplayName`, `DisplayVersion`, `Publisher` = "SyncThing V2 contributors"
     - `DisplayIcon`, `InstallLocation`
     - `UninstallString` = `"…\stv2.exe" uninstall`
     - `QuietUninstallString` = `… uninstall --yes`
     - `NoModify=1`, `NoRepair=1`, `EstimatedSize`
  4. Create the Start Menu shortcut `%APPDATA%\Microsoft\Windows\Start Menu\Programs\SyncThing V2.lnk` through `IShellLinkW` (go-ole).
  5. Set the tray autostart (Run value).
  6. Run **legacy migration** (§6.5).
  7. Unless `--no-start`, launch `stv2.exe --setup` detached.
- **The welcome view** then:
  - checks Tailscale (offering the install card)
  - detects or bootstraps Syncthing (§5)
  - offers `firewall allow`
  - goes to Pair
- **`stv2 uninstall`:**
  - stops the tray
  - removes the Run values that SyncThing V2 owns, the Start Menu shortcut, the Uninstall key and `%LOCALAPPDATA%\SyncThingV2` (prefs, logs, WebView2 data)
  - asks whether to also remove the **managed** Syncthing binary and its autostart. It never deletes Syncthing's config, database or synced folders.
  - self-deletes with a detached hidden `cmd.exe /c ping -n 3 127.0.0.1 >nul & rmdir /s /q "<install dir>"`
- **Exe resources** come from `go-winres` using `packaging/windows/winres.json`:
  - the icon (`assets/icons`)
  - version info
  - `app.manifest`: `asInvoker`, PerMonitorV2 DPI awareness, Common Controls v6, `supportedOS` Windows 10/11
- **Supported:** Windows 10 1809+ and Windows 11, x64. Arm64 runs the x64 build under emulation.

### 6.2 macOS

- **Artifacts:**
  - `SyncThingV2-<v>-macos-universal.dmg`, a drag-to-Applications image built with `hdiutil`
  - `SyncThingV2-<v>-macos-universal.tar.gz`, containing `SyncThing V2.app`
- **Bundle:**
  - `Contents/MacOS/stv2` (a universal binary made with `lipo`)
  - `Contents/Resources/stv2.icns`, built with `iconutil` from `assets/icons`
  - `Info.plist`: `LSUIElement=true`, `CFBundleIdentifier=io.github.neutx.syncthingv2`, `LSMinimumSystemVersion=12.0`, `NSHighResolutionCapable=true`
  - Ad-hoc signed: `codesign --force --sign - --timestamp=none` on the binary, then on the bundle.
- **First launch from anywhere:**
  - With no prefs, `--setup` behaviour runs automatically: the welcome view asks to enable "Open at Login" (LaunchAgent), then bootstraps Syncthing and pairing.
  - `stv2 install --yes`, used by `install.sh`, does the same without prompts.
  - `stv2 uninstall` removes the LaunchAgents SyncThing V2 owns, `~/Library/Application Support/SyncThingV2` (except the managed Syncthing, unless `--remove-syncthing`) and the `.app`, when it is in `~/Applications`.
- **Gatekeeper (unsigned):**
  - The DMG path needs System Settings → Privacy & Security → **Open Anyway**. This is the macOS 15+ flow; right-click → Open no longer works.
  - The `curl | sh` path downloads without quarantine and is the recommended one.

### 6.3 Linux

- **Artifacts:**
  - `SyncThingV2-<v>-linux-{x64,arm64}.tar.gz`: `stv2`, `LICENSE`, `THIRD_PARTY_NOTICES.md`
  - `syncthing-v2_<v>_{amd64,arm64}.deb` (nfpm): `/usr/bin/stv2`, `/usr/share/applications/stv2.desktop`, and hicolor icons at 16–512
- **`stv2 install`** (tarball or one-liner):
  - copies itself to `~/.local/share/syncthing-v2/bin/stv2`
  - symlinks `~/.local/bin/stv2`
  - writes `~/.local/share/applications/stv2.desktop` and the icons (embedded in the binary through `go:embed`), then the autostart entries (§5.3)
  - **With the `.deb`,** the same per-user step runs on first launch through the welcome view.
- **Runtime:**
  - The tray needs a StatusNotifierItem host. GNOME needs the "AppIndicator and KStatusNotifierItem Support" extension.
  - If `org.kde.StatusNotifierWatcher` has no owner on the session bus, the app sends a notification explaining this, opens the dashboard in the browser, and doctor reports UI002.
- **Supported:** x64 and arm64 glibc or musl distros (the binary is static), on X11 or Wayland.

### 6.4 Dashboard host details

**Windows** (`host_windows.go`, `placement_windows.go`):

- **Window:** a Win32 popup (`WS_POPUP`, `WS_EX_TOOLWINDOW | WS_EX_TOPMOST`) with class style `CS_DROPSHADOW`.
  - Region: `SetWindowRgn` with a squircle polygon (radius 40 × scale, n = 4, from `glass.SquirclePath`).
  - WebView2 through `go-webview2/pkg/edge`, with its user-data folder at `%LOCALAPPDATA%\SyncThingV2\WebView2`.
  - Hides on `WM_ACTIVATE` / `WA_INACTIVE` and on Esc (the web page posts `hide`).
- **Placement:** on the monitor under the cursor, at the edge where `WorkingArea` differs from `Bounds` (the taskbar side), inset 14 px × scale. The size is 460×640 DIP × the monitor scale.
- **Show sequence** (tray process):
  1. `Place`
  2. `glass.Capture(rect)` while the popup is hidden (if the host was visible, `hide` is sent first and the capture waits 45 ms)
  3. `glass.Render`
  4. cache the PNG for `/api/backdrop.png`
  5. send `show x y w h`
  6. the page fetches the backdrop and plays the entrance animation
- **WebView2 missing:** `edge` initialisation fails, so the host exits with code 3. The tray then switches to **browser fallback** (the Linux path) permanently for the session, and doctor reports UI001.

**macOS** (`host_darwin.m`):

- **Window:** an `NSPanel` (`NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel`, floating level, `hidesOnDeactivate`). Its content view is an `NSVisualEffectView` (material `HUDWindow`, blending `behindWindow`, state `active`, corner radius 22 with a mask image). A `WKWebView` with `drawsBackground=NO` sits on top.
- **Placement:** top-right of the screen under the mouse, 8 pt below the menu bar.
- **Dismissal:** closes on `resignKey` or Esc.

**Linux** (`host_linux.go`):

1. Create `$XDG_RUNTIME_DIR/stv2/` with mode 0700 (fallback `~/.cache/stv2/`), and in it `open-<rand>.html` with mode 0600.
   - A strictly confined snap browser (the default Firefox on Ubuntu) cannot read either place. When `xdg-mime query default text/html` names a snap desktop file (one in `/var/lib/snapd/desktop/applications`), the directory is the snap's own `~/snap/<instance>/common/stv2/` instead, which the snap may read. The token still never appears in a command line; a GET login URL was rejected for that reason.
2. The file auto-submits `POST http://127.0.0.1:<port>/login` with a single-use launch token.
3. Run `xdg-open <file>`, then delete the file after 30 s.
4. The server accepts `Origin: null` **only** on `/login` with a valid, unexpired token. It then sets the cookie and redirects to `/?mode=browser`.

### 6.5 Legacy prototype migration (Windows)

1. **Detect:** `Startup\Syncthing Tray.lnk` exists, or a process `SyncthingTray.exe` is running.
2. **Ask once** (dashboard notice `legacy-tray`): "An older Syncthing tray app is running. Replace it with SyncThing V2?"
3. **On yes:** terminate that process and rename the `.lnk` to `Syncthing Tray.lnk.disabled` (reversible; documented in `docs/migrating-from-manual-setup.md`).
4. **On no:** do nothing and never ask again.
5. An existing `Startup\Syncthing.lnk` counts as Syncthing autostart already configured.

---

## 7. One-liner install scripts

### 7.1 Windows: `scripts/install.ps1`

**Documented one-liner.** The TLS prefix is mandatory, because stock PowerShell 5.1 may not offer TLS 1.2 to GitHub:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; irm https://github.com/Neutx/syncthing-v2/releases/latest/download/install.ps1 | iex
```

**Script** (PowerShell 5.1 compatible, fewer than 100 lines, `Set-StrictMode -Version Latest`, `$ErrorActionPreference='Stop'`):

1. Set TLS 1.2 again, for when the script is run directly.
2. `$ver` is baked in at release time (the placeholder `__STV2_VERSION__` is replaced by `release.yml`). `$env:STV2_VERSION` overrides it.
3. `$base` defaults to `https://github.com/Neutx/syncthing-v2/releases/download/v$ver`. `$env:STV2_BASE_URL` overrides it (used by CI).
4. Refuse to run on 32-bit Windows. On arm64, print "running x64 build under emulation".
5. Download `SyncThingV2-Setup-$ver-windows-x64.exe` and `SHA256SUMS.txt` into a new `%TEMP%\stv2-<guid>` directory with `Invoke-WebRequest -UseBasicParsing`.
6. Compare `Get-FileHash -Algorithm SHA256` with the matching line. On mismatch, throw and delete the directory.
7. Run `Unblock-File`, then `& $exe install --yes`. Check `$LASTEXITCODE`.
8. Clean up the temp dir in `finally`.
9. Print the next steps: "Open SyncThing V2 from the tray. Do the same on your other computer, then click Pair."

The script never elevates and never dot-sources other files, so it works under any execution policy when piped to `iex`.

### 7.2 macOS and Linux: `scripts/install.sh`

**Documented one-liner:**

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh
```

**Script** (POSIX sh, `set -eu`, passes shellcheck):

1. Refuse to run as root unless `--allow-root` is given.
2. Detect the platform with `uname -s` and `uname -m` (Darwin → universal; Linux x86_64 → x64, aarch64/arm64 → arm64). Anything else exits with an error message.
3. The version is baked in the same way, with `STV2_VERSION` and `STV2_BASE_URL` overrides.
4. Download the tarball and `SHA256SUMS.txt` into `mktemp -d`, removed through `trap`.
5. Verify with `sha256sum -c`, or with `shasum -a 256 -c` on macOS.
6. **Install:**
   - macOS: extract, move `SyncThing V2.app` to `~/Applications/` (replacing an older copy after quitting it with `stv2 --quit`, which uses the control token), then run `".../stv2" install --yes`.
   - Linux: extract `stv2`, then run `./stv2 install --yes`.
7. Support `--uninstall`, which runs `stv2 uninstall --yes`.
8. It never runs sudo.

---

## 8. Dashboard UI (`internal/ui/web`)

### 8.1 Structure and parity

The dashboard has three views: **Status** (the default, with prototype parity), **Pair**, and **Settings/About**.

- **Status view:**
  - header: glyph, headline, subline
  - Connection, Sync Progress and Transfer cards
  - Recent Activity
  - 5 buttons
  - notice banners at the top, for pending prompts, GUI exposure, legacy migration, updates and Tailscale
- **Pair view:**
  - the candidate list, grouped "Your devices" and "Other people's devices"
  - pending requests with Accept and Decline
  - Share-a-folder
- **Settings/About:**
  - autostart toggles (tray and Syncthing)
  - transport profile (with the trade-off text)
  - notifications on or off
  - update check on or off
  - "Copy diagnostics"
  - "Open logs folder"
  - version and the disclaimer

**Implementation notes:**

- **Canvas:** the status glyph (36 px ring, with all D5 states including pause bars and a specular glint) and the progress bar (spring-driven, with shimmer while syncing or scanning, and a top highlight) are drawn on `<canvas>`, sized with `devicePixelRatio`.
- **`anim.js`** is a one-to-one port of `Anim.cs` (Ease, Spring, Tween, Loop, ColorLerp). The animation loop runs on `requestAnimationFrame` and stops when nothing is moving, except for the presence pulse while a peer is connected.
- **`tokens.css`** holds the D12 tokens verbatim:
  - Colours: Fg `#F6F8FC`, Dim `#BCC2CE`, Faint `#9AA3A8`, Accent `#60A6FF`
  - State colours: `#3AC672`, `#569CF6`, `#F0B838`, `#EE5C52`, `#96969E`, `#787C86`
  - Grid: PadX 30, ColL 46, ColR 414, Col2 230
  - Type sizes: 15.5, 11.5, 9, 8.5 and 7.5 pt
  - Fonts: `"Segoe UI", -apple-system, "SF Pro Text", Cantarell, Inter, "DejaVu Sans", sans-serif`; mono `Consolas, "SF Mono", Menlo, "DejaVu Sans Mono", monospace`
- **Motion:**
  - Entrance: OutBack over 0.34 s. Scale goes from 0.965 to 1 with `transform-origin` at the tray corner, content rises 12 px, and the backdrop opacity ramps up.
  - Exit: OutCubic over 0.18 s, then `hide`.
  - The state colour lerps, and the sweep replays when the state changes.
- **Buttons** (D10): Rescan, Pause/Resume, Folder, Web UI, Restart. Rescan, Pause and Restart are disabled in Down and Unauthorized. A press gives a spring "gel press" to 0.95 scale on `:active` / pointerdown. A click fires only when the pointer is released inside the button.
  - **Hover changes only the fill and rim brightness. There is no scale, translate, shadow raise or elevation on hover.**
  - This is enforced by `internal/ui/hover_lint_test.go`: it parses every CSS rule whose selector contains `:hover` and fails on `transform`, `translate`, `scale`, `box-shadow`, `top`, `margin-top` or `filter: drop-shadow`. It also fails if `app.js` uses `mouseenter` or `mouseover` handlers that write `style.transform`.
- **Folder button:** with more than one folder, it opens a small in-dashboard list.
- **Materials:**

| Mode | Material |
|---|---|
| Windows | backdrop image (glass) plus the scrim, per-card legibility floor (alpha 104 dark fill), rim and breathing specular arc (D4) |
| macOS (`?mode=vibrancy`) | transparent body over the native blur, with the same scrim and cards |
| Linux and fallback (`?mode=browser`) | a centred 460×640 card with a static dark gradient material, the same cards, and no capture |

- **Accessibility:** buttons are real `<button>` elements with labels; the focus ring is visible; `prefers-reduced-motion` disables the sweep, shimmer and springs.

### 8.2 Tray surface (F19–F28 parity)

- **Icon:** `icon.Ring` at the tray size (Windows: `SM_CXSMICON` × DPI; macOS: 18 pt @2x; Linux: 22/24 px PNG).
  - Colours as in the prototype: InSync `#2EBE64`, Syncing `#3896F0`, Scanning `#F0B428`, NoPeer and Error `#E65046`, Paused `#96969B`, Down and Unauthorized `#6E6E73`.
  - The glyphs are the same as the prototype's, plus Unauthorized, which shows an X in the Down colour.
  - On Windows, the old HICON is destroyed with `DestroyIcon` after each change.
- **Tooltip:** follows the F20 rules. Line 1 reads "SyncThing V2 - <Label>", line 2 is PeerLine, and line 3 is FolderLine, becoming `FolderLine  ETA x\n SpeedLine` while syncing and moving. It is capped at 127 characters with "...".
- **Menu:**
  1. Three disabled info rows (state and folder line; transfer and ETA; startup status)
  2. Open Status Window
  3. Pair Devices…
  4. Open Web UI
  5. Open Sync Folder (a submenu when there are several folders)
  6. Rescan All
  7. Pause All / Resume All
  8. Restart Syncthing
  9. Start Syncthing (shown only in Down)
  10. "Start SyncThing V2 <at login wording>" (checked)
  11. "Start Syncthing <at login wording>" (checked; disabled when configured outside SyncThing V2)
  12. Update available (vX)… (only when there is one)
  13. About
  14. Exit
- **At-login wording** comes from `brand.AtLogin()`: "with Windows" on Windows, "at login" on macOS, "on login" on Linux.
- **Click:**
  - Windows: a left click and a balloon click open the dashboard.
  - macOS and Linux: a click opens the menu (platform convention), and "Open Status Window" is the first actionable item.
- **Notifications** use the F23 transitions unchanged ("Syncthing disconnected", "Syncthing stopped", "Syncthing error", "Syncthing in sync"), plus:
  - "API key rejected"
  - pairing prompts
  - folder-share prompts
  - "Update available" (once per version)

  They can be disabled in Settings. Windows uses tray balloons (`NIF_INFO`, 6 s); Linux uses D-Bus with a `default` action that opens the dashboard; macOS uses `osascript` via argv (a click does not open the dashboard; this is documented).

### 8.3 Loopback server hardening

- **Listener:** binds `127.0.0.1:0` only.
- **Host check:** every request must carry `Host == 127.0.0.1:<port>`. Anything else gets a 421 or 403, which defeats DNS rebinding.
- **Origin check:** a present `Origin` must be `http://127.0.0.1:<port>`. The single exception is `Origin: null` on `/login` with a valid token.
- **Session:**
  - The cookie `stv2s` holds a random 256-bit session value, with `HttpOnly; SameSite=Strict; Path=/`.
  - Sessions are in memory and rotate on each tray start.
  - Launch tokens are single-use and expire after 30 s.
- **POSTs** require `X-STV2: 1`, including the bearer control routes.
- **`GET /login?token=`** is accepted alongside `POST /login` for printed links. It has the same single-use, 30 s rules, and `Referrer-Policy: no-referrer` keeps the token out of referrers.
- **No CORS headers ever.** Responses also carry these headers:
  - `Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'`
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: no-referrer`
- **`/api/show`, `/api/quit` and `/api/launch`** accept only `Authorization: Bearer <control token>`. That token is written to `instance.json` (mode 0600, in the per-user app dir) together with the port and PID.

---

## 9. CI and release pipeline

All workflows follow the same rules:

- Actions are pinned to full commit SHAs, and Dependabot tracks `github-actions` and `gomod` weekly.
- Top-level `permissions: contents: read`.
- Go is set up with `actions/setup-go`, using `go-version-file: go.mod`.

### 9.1 `ci.yml` (push to main, pull requests)

| Job | Runner | Steps |
|---|---|---|
| lint | ubuntu-latest | `gofmt -l` is empty; `go vet ./...`; staticcheck; govulncheck; `go mod tidy -diff`; shellcheck (`scripts/*.sh`, `packaging/**/*.sh`); actionlint; gitleaks (full history); `scripts/privacy-check.sh` with the `PRIVACY_DENYLIST` secret (skipped with a notice on forks where the secret is absent) |
| lint-ps | windows-latest | PSScriptAnalyzer on `scripts/*.ps1` (Error severity fails) |
| test | matrix windows-latest, ubuntu-latest, macos-14 | `go test -race ./...` (includes the hover lint, device-ID vectors, policy truth table, the probe against an in-process TLS server, and the fake-REST engine tests) |
| e2e | matrix ubuntu-latest, windows-latest | `go test -tags e2e -timeout 15m ./e2e/...` |
| build | matrix: the three OSes | `scripts/build.sh` / `scripts/build.ps1` smoke build; upload the artifacts |
| installer-test | matrix: the three OSes; needs build | Serve the artifacts with `python -m http.server 8000`. Run the one-liner against `STV2_BASE_URL=http://127.0.0.1:8000`. On Windows it runs inside `powershell -NoProfile -ExecutionPolicy Restricted -Command "<exact documented one-liner, URL swapped>"`. Then check: `stv2 version`; `stv2 doctor --json`, whose `install.*` checks must pass (Tailscale checks are expected to fail on runners and are ignored); the autostart entry exists; `stv2 uninstall --yes` removes it. |

### 9.2 `release.yml` (tag `v*.*.*`)

1. **verify** (ubuntu): the tag matches semver, `CHANGELOG.md` has a `## [x.y.z]` section, and every job from ci.yml has passed on this commit (it re-runs lint, test and e2e through `workflow_call`).
2. **build-windows** (windows-latest):
   - `go run github.com/tc-hib/go-winres@<pin> make --in packaging/windows/winres.json --product-version <v> --file-version <v>`
   - `go build -trimpath -ldflags "-s -w -H windowsgui -X …/brand.Version=<v>" -o SyncThingV2-Setup-<v>-windows-x64.exe ./cmd/stv2`
3. **build-linux** (ubuntu-latest):
   - `CGO_ENABLED=0 GOARCH={amd64,arm64}` builds, packed into tarballs
   - `go run github.com/goreleaser/nfpm/v2/cmd/nfpm@<pin> package -p deb` for each arch
4. **build-macos** (macos-14):
   - two cgo builds (arm64 native; amd64 with `CC="clang -arch x86_64"`), `lipo -create`
   - `packaging/macos/make-app.sh` (Info.plist, icns, ad-hoc codesign)
   - `make-dmg.sh`, plus the tarball
5. **installer-test:** the same as the CI job, against these exact artifacts.
6. **publish** (ubuntu; `permissions: contents: write, id-token: write, attestations: write`):
   - bake the version into `install.ps1` and `install.sh`
   - write `SHA256SUMS.txt` over every asset, including both scripts
   - run `actions/attest-build-provenance` on every asset
   - `gh release create v<v> --draft --title "SyncThing V2 <v>" --notes-file <CHANGELOG section>`, then upload
7. **Conditional signing steps,** present but skipped unless their secrets exist:
   - `SIGNPATH_API_TOKEN` (Authenticode through SignPath Foundation)
   - `APPLE_DEVELOPER_ID_P12`, `APPLE_ID`, `APPLE_TEAM_ID`, `APPLE_APP_PASSWORD` (codesign plus `notarytool`)

**Reproducibility:** `-trimpath`, `-buildvcs=true`, and `SOURCE_DATE_EPOCH` = the commit time.

### 9.3 Human release checklist (`docs/releasing.md`)

1. Upload the Windows exe to VirusTotal. If there are detections, submit it to the Microsoft Defender false-positive portal and wait for clearance.
2. Smoke-install from the draft assets on a clean Windows VM or Sandbox, plus a macOS and a Linux machine if available.
3. Run the live two-computer pairing test over Tailscale (§10.5).
4. Publish the draft. The one-liners resolve `releases/latest` only once the release is published.

### 9.4 Local build (Windows, only Go installed)

`scripts/build.ps1` runs go-winres, then `go build`, then `go test ./...`, and writes `dist/stv2.exe`.

`scripts/build.sh` does the same for Unix and CI.

---

## 10. Testing strategy

### 10.1 Unit tests (all OSes)

- **status:**
  - every state-machine rule, including Disconnected-with-need
  - the percent rule and rate deltas with a counter reset
  - ETA and every formatter, as golden strings taken from prototype behaviour
  - `Classify` over each prefix edge, relay-first, and IPv6 zones
  - `Describe` for every event type in the inventory
  - `NoticeFor` transitions
  - `Diagnostics` redaction (no API key; IDs cut to 7 characters)
- **stclient:**
  - discovery order, using a fake `syncthing` script and env
  - wildcard normalisation
  - `tls=true` with a CA pool
  - typed errors against `httptest` (401, 403, refused, bad JSON)
  - `Audit` cases
- **engine:** against a fake REST and event server (`httptest`): long-poll seed, back-off and re-seed, `FolderSummary` handling, and reconcile.
- **tailnet:** parsing synthetic status JSON (NeedsLogin, sharee, tagged, offline, other owner, the trailing dot), and `Locate` with a fake PATH.
- **deviceid:**
  - upstream's documented vector strings, for the Luhn and chunk format
  - a committed synthetic certificate with its expected ID
  - `e2e` cross-checks against a real `syncthing device-id`
- **pairing:**
  - `Decide` truth table: non-tailnet IP, unknown node, probe mismatch, probe error, same owner and other owner
  - `Probe` against an in-process `tls.Listen` server (TLS 1.3, `RequireAnyClientCert`) that asserts **no client certificate is ever received**
  - `ShareFolder` merge idempotency
  - `SafeFolderDir` against traversal, reserved names and collisions
- **stinstall:** pin mismatch aborts; zip-slip and tar-slip are rejected; version comparison.
- **autostart:** round-trips against temp HOME, APPDATA and registry (on Windows, a test-only HKCU subkey passed as a parameter).
- **tray:** `BuildMenu` and `Tooltip` goldens (127-character cap, 4-line syncing case).
- **icon:** a pixel snapshot per state.
- **glass:** golden images with a tolerance (±2 per channel) on a fixed synthetic 64×64 input; a benchmark with a budget of ≤ 120 ms at 460×640.
- **ui:** auth matrix (bad Host, bad Origin, missing cookie, missing `X-STV2`, token reuse, expired token → 403); CSP header present; the hover lint.
- **doctor:** every code, using fakes.

### 10.2 Test seams (no production test flags)

- `tailnet.Source` is an interface: the CLI in production, a fake in tests.
- `pairing.Policy.Prefixes` is a parameter: e2e passes `127.0.0.0/8`.
- `pairing.Service.Probe` is a function field.
- Autostart, install and uninstall roots are parameters.
- Nothing is read from environment variables for test purposes in the shipped binary. The only env variables are `STV2_VERSION` and `STV2_BASE_URL`, and they are read by the install **scripts**.

### 10.3 E2E (`e2e/`, tag `e2e`; ubuntu and windows)

1. Get the pinned Syncthing through `stinstall.Download` into a cached temp dir.
2. Start two instances with separate `--home` dirs:
   - GUI on 127.0.0.1:18384 and 127.0.0.1:28384
   - listeners `tcp://127.0.0.2:22000` and `tcp://127.0.0.3:22000`
   - local and global discovery and relays off
3. Build a fake `tailnet.Source` that maps 127.0.0.2 and 127.0.0.3 to same-owner nodes, and one mapping to an "other owner".
4. **Assert:**
   1. `Probe` returns each instance's `syncthing device-id`.
   2. **After the probes, `/rest/cluster/pending/devices` on both instances is empty.**
   3. A `Pair`s B, so B has a pending A, and `Decide` returns `prompt` with Verified.
   4. `Accept` on B brings both connected within 60 s.
   5. `ShareFolder` on A leads to a pending folder on B; `AcceptFolder` into `SafeFolderDir`; a file written on A appears on B within 60 s.
   6. `Widen` adds `dynamic`.
   7. Pairing both sides simultaneously gives no duplicate devices.
   8. The mismatch case: pending from an IP whose probe returns another ID gives `ignore`.

### 10.4 Local Windows verification (maintainer machine)

- `scripts\build.ps1`
- `go test ./...` and `go test -tags e2e ./e2e/...`
- Run the tray against a **sandbox** Syncthing: separate `--home`, GUI 18384, listener 22001.
- Run a side-by-side parity check against the prototype (§13), with screenshots.
- `stv2 pair --list` against the real tailnet.
- The live config is never modified during tests. The GUI-exposure prompt is declined.

### 10.5 Release gate

- CI is green on all three OSes, including installer-test.
- The local Windows build passes.
- **The live two-computer test passes:** install on both machines through the one-liner → Pair from one → Accept on the other → share a folder → a file syncs both ways → a reboot keeps sync.
- The README labels Windows **Stable** and macOS and Linux **Preview**, until someone confirms them on real hardware (a tracking issue template asks for reports).

---

## 11. Documentation plan

| File | Contents |
|---|---|
| README.md | Pitch; 3-step quick start (install Tailscale on both → run the one-liner on both → Pair and Accept); the three one-liners; real demo-mode screenshots; feature list; platform status table (Windows Stable; macOS and Linux Preview); verifying downloads (SHA256SUMS, `gh attestation verify`); links to docs; disclaimer |
| docs/install-windows.md | One-liner (with the TLS prefix explained); exe and SmartScreen "More info → Run anyway"; what is installed where; firewall (`stv2 firewall allow`); upgrade; uninstall |
| docs/install-macos.md | One-liner (recommended); DMG with "Open Anyway" (macOS 15+); Open at Login; application firewall; uninstall |
| docs/install-linux.md | One-liner; `.deb` (`sudo apt install ./syncthing-v2_…deb`); tarball; the GNOME AppIndicator extension; browser dashboard; systemd user unit; uninstall |
| docs/tailscale-setup.md | Install and sign in on both machines; same account vs. sharing; the ACL grant `{"grants":[{"src":["autogroup:member"],"dst":["autogroup:self"],"ip":["tcp:22000","udp:22000"]}]}` plus the legacy `acls` equivalent; shields-up; MagicDNS not required; transport profiles and their trade-off |
| docs/pairing.md | The flow from §4 in plain words; what each status means; manual fallback through the Syncthing web UI; non-default ports |
| docs/architecture.md | §2 diagram and components, the process model, data flow |
| docs/security.md | §4.6 threat model, what is exposed and what is not, loopback server, supply chain, privacy of the update check |
| docs/troubleshooting.md | One section per doctor code (§3.11), SmartScreen and Gatekeeper, the no-tray-icon case on GNOME, logs location, "Copy diagnostics" |
| docs/building.md | Go toolchain, `build.ps1` and `build.sh`, tests, e2e, demo mode, screenshots |
| docs/releasing.md | Tagging, CHANGELOG, draft review, the §9.3 checklist, bumping the Syncthing pin |
| docs/uninstall.md | Per OS; what is kept (Syncthing config and data) |
| docs/migrating-from-manual-setup.md | Adopting an existing Syncthing; the legacy tray `.lnk.disabled`; how to revert |
| docs/design.md | The Liquid Glass spec reconciled to the code constants (blur r=5 on half-res, 2 passes, saturation 2.0, radius 40, IOR 1.5, etc.); materials per OS; **no hover-raise rule** |
| CONTRIBUTING.md | Setup, tests, style (gofmt, staticcheck), the UI no-hover-raise rule, the privacy hook (`git config core.hooksPath .githooks`), the PR checklist |
| SECURITY.md | Private reporting through GitHub Security Advisories; supported versions (latest minor); scope |
| CHANGELOG.md | Keep a Changelog, starting at `## [1.0.0]`, listing the deliberate behaviour fixes relative to the prototype |
| THIRD_PARTY_NOTICES.md | Each Go module with its licence, and Syncthing (MPL-2.0; downloaded, not bundled; link to the exact source tag) |
| LICENSE | MIT, copyright "SyncThing V2 contributors" |

**Screenshots:** `scripts/screenshots.ps1` starts `stv2 demo --port 18999`, which serves synthetic data and a glass backdrop rendered from a synthetic gradient. It then runs `msedge --headless=new --window-size=460,640 --screenshot=assets/screenshots/<view>.png http://127.0.0.1:18999/?demo=<view>` for the status, syncing, pair and settings views, and renders the tray icon strip with `go run ./internal/icon/cmd/genicons --strip`. No real names, IPs, files or desktop appear.

---

## 12. Privacy guard

- **`scripts/privacy-check.sh` / `.ps1`:**
  - reads newline-separated literal patterns from `$PRIVACY_DENYLIST` (a CI secret) or from `.git/info/privacy-denylist`, a local file that is never tracked
  - greps the tracked tree with `git grep -F -i -f`, and, when `--history` is passed, the full history with `git log -p`
  - fails with the matching file and line **without printing the pattern**
- **Pre-commit:** `.githooks/pre-commit` runs the check on staged content, plus a gitleaks scan when gitleaks is installed.
- The pattern list itself is **never committed**. It covers the maintainer's tailnet name, tailnet IPs, device IDs, folder IDs, hostnames, email local-part and personal paths.
- Fixtures use RFC 5737 / 100.64.0.x documentation addresses, generated device IDs and `example` hostnames.

---

## 13. Feature-parity checklist against the prototype

Every row must be ticked before release. The side-by-side check uses the prototype exe and V2 against the same sandbox Syncthing.

| ID | Prototype behaviour | V2 implementation | Change |
|---|---|---|---|
| F1 | Single instance through a named mutex | `single` (`Local\SyncThingV2.Tray`; flock on Unix) plus second-launch → show dashboard | Different name; added "show" |
| F2 | config.xml API key and address normalisation | `stclient.Discover` / `ReadConfig` | Portable discovery; TLS support |
| F3 | X-API-Key; timeouts 5/60/6 s | `stclient.Client` | — |
| F4 | 2 s poll, plus immediate refresh on start, dashboard open and each command | `status.Engine` | Folder status is event-driven with a 30 s reconcile |
| F5 | Non-overlapping refresh, results marshalled | single-owner goroutine | Fixes the thread-safety bug |
| F6 | Event long-poll: seed, then since/timeout=50, 3 s back-off | `Engine` | — |
| F7 | Exit | tray Exit → stop engine, server and host | Syncthing keeps running (unchanged) |
| F8 | Device name map, 7-character fallback | `Engine` | — |
| F9 | Excludes own `myID` | `Engine` | — |
| F10 | Rate deltas over >0.5 s with a reset guard | `Engine` | — |
| F11 | Connected peers and transport | `Classify` with CIDR, all peers | Fixes classifier gaps and last-peer-only |
| F12 | Folder aggregation | `Engine` | All folders kept individually and in aggregate |
| F13 | Percent rule | `status` | — |
| F14 | State priority | §3.3 | NoPeer above Syncing-with-need; Unauthorized added |
| F15 | PeerLine, FolderLine, FilesLine, SizeLine | `status` | Generalised copy |
| F16 | Speed line and ETA | `status` | — |
| F17 | Unused Detail block | "Copy diagnostics" (redacted) | Now used |
| F18 | Grp, Rate, Size, Label | `status` | — |
| F19 | Runtime ring icon, DestroyIcon | `icon` + `tray` | Unauthorized glyph |
| F20 | 127-character tooltip | Native `szTip` on Windows | No reflection hack |
| F21 | Left-click or balloon click opens the dashboard | Windows native; menu item on macOS and Linux | Platform convention |
| F22 | Context menu | §8.2 | Added Pair, Syncthing autostart toggle, Update, About |
| F23 | Balloon transitions | `NoticeFor` + `notify` | Added pairing and API-key notices |
| F24 | Open Sync Folder | Submenu for several folders | — |
| F25 | Start Syncthing | Located binary; offered only in Down | Fixes double-start on auth errors |
| F26 | Tray autostart `.lnk` | HKCU Run / LaunchAgent / XDG | Mechanism changed |
| F27 | Pause/Resume all | `Engine` commands | — |
| F28 | Activity notes | `Engine.Note` | — |
| §2.4 | Event → activity map and tints | `status.Describe` | — |
| D1 | 460×640 borderless popup, squircle 40, drop shadow, no taskbar entry | `uihost` Windows | DPI-scaled, placed at the taskbar edge |
| D2 | Esc, focus loss, close → hide | all hosts | — |
| D3 | Glass backdrop pipeline | `glass` (Go port) | macOS vibrancy; Linux flat |
| D4 | Scrim, sweep, legibility floor, rim, breathing arc | CSS/canvas | — |
| D5 | Header glyph, headline, subline | canvas + DOM | Generalised copy |
| D6 | Connection card | DOM | "+N more" for several peers |
| D7 | Sync Progress card and spring bar with shimmer | canvas | — |
| D8 | Transfer card, meters (8 MB/s full scale), startup dots | DOM | Per-OS wording |
| D9 | Recent Activity: 40 kept, 4 shown, slide-in | DOM | — |
| D10 | Five buttons with gel press | DOM | **Hover scale removed** |
| D11 | Motion timings and easing | `anim.js` | `prefers-reduced-motion` |
| D12 | Tokens, grid, fonts | `tokens.css` | Font fallbacks per OS |

---

## 14. Risks and mitigations

| Risk | Likelihood / impact | Mitigation |
|---|---|---|
| Heuristic antivirus flags the unsigned Go self-installer | Medium / high | VirusTotal and Defender submission before publishing; the one-liner path; SignPath application after 1.0 |
| macOS and Linux tray or dashboard behaviour is unverified on real hardware | High / medium | Labelled Preview; CI builds and headless tests; issue template for reports |
| `go-webview2` or WebView2 runtime problems on some Windows 10 builds | Low / medium | Browser fallback (UI001) |
| `fyne.io/systray` Linux SNI differences between desktops | Medium / low | Menu carries all the information; Open Status Window item; UI002 guidance |
| Windows Firewall blocks inbound 22000 | Medium / medium | One-way dial is enough; `firewall allow`; NET001 and FW001 |
| Tailnet ACLs block 22000 | Medium (non-default tailnets) / high | NET001, the ACL grant in the docs |
| Peers with a non-default Syncthing listen port | Low / medium | ST005; pairing docs describe the manual fallback |
| Glass port performance at high DPI | Low / low | Cached displacement map, half-res blur, benchmark budget |
| An upstream Syncthing API change | Low / medium | Typed `ErrBadResponse`; e2e runs against the pinned version; `MinSyncthing` check |
| The name or trademark objection | Medium / medium | Disclaimer; no logo; one-file rename (`brand.go`); decision D1 |
| A privacy leak of maintainer data | Low / high | §12 guard in CI and pre-commit; synthetic fixtures |

---

## 15. Decisions the maintainer must confirm before public release

These do not block implementation. Each has a default that is already wired in.

| # | Decision | Default in this spec |
|---|---|---|
| D1 | Product name (conflicts with upstream Syncthing v2.x; trademark) | "SyncThing V2" with the disclaimer; rename through `brand.go` |
| D2 | Repository location inside a Syncthing-shared folder | Recommended: add an ignore entry for the repo's build and VCS dirs, or keep the working copy outside the share |
| D3 | Git commit identity | Recommended: a repo-local GitHub noreply email |
| D4 | Default transport profile for new installs | `tailnet` |
| D5 | Licence | MIT |
| D6 | Code-signing budget (SignPath Foundation, Apple Developer ID) | Unsigned 1.0 with conditional steps pre-wired |
