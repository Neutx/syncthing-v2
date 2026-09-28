# Architecture

SyncThing V2 is one Go binary, `stv2` (`stv2.exe` on Windows), per OS and architecture. It has no runtime to install. The dashboard is plain HTML, CSS and JavaScript, embedded in the binary. Syncthing and Tailscale are separate programs that SyncThing V2 talks to. It never contains them.

## Process model

```
                 ┌──────────────── stv2 (tray process; main thread = tray loop) ────────────────┐
 OS autostart ──►│ app.App                                                                       │
 (--background)  │  ├─ status.Engine  ── poll 2 s + /rest/events long-poll ──► Syncthing REST    │
                 │  │     └─ publishes model.Snapshot (immutable) values                         │
                 │  ├─ tray.Tray (native Win32 on Windows; fyne.io/systray on macOS and Linux)   │
                 │  ├─ notify (tray balloons / osascript argv / D-Bus)                           │
                 │  ├─ pairing.Watcher (PendingDevicesChanged, PendingFoldersChanged)            │
                 │  ├─ ui.Server 127.0.0.1:0 (SSE /api/state, POST /api/action; token + cookie)  │
                 │  ├─ glass.Render (Windows only: desktop capture → refracted backdrop PNG)     │
                 │  └─ update.Checker (daily, read-only GitHub Releases query)                   │
                 └──────────────▲────────────────────────────────────────────────────────────────┘
                                │ stdin line protocol: "auth <token>", "show x y w h", "hide", "quit"
                 ┌──────────────┴──── stv2 ui-host (Windows only; child process, prewarmed) ──────┐
                 │ WebView2 in a borderless, squircle-clipped Win32 popup                         │
                 └────────────────────────────────────────────────────────────────────────────────┘
                 macOS and Linux: no ui-host. A 0600 launch file with a single-use token is opened
                 in the default browser (`open` on macOS, `xdg-open` on Linux).

 Syncthing: its own process, started by OS autostart (HKCU Run / LaunchAgent / systemd --user
 or XDG autostart). It is never a child of the tray, so a tray crash or exit never stops sync.
 Tailscale: read-only CLI calls (`tailscale status --json`), with a 5 s timeout.
```

- **One process per user.** A second launch finds the running tray through the single-instance lock and `instance.json`. It asks the running tray to show its dashboard, then exits. The lock is a named mutex (`Local\SyncThingV2.Tray`) on Windows and `flock` elsewhere.
- **Syncthing is independent.** Exiting SyncThing V2 stops the engine, the dashboard server and the dashboard host. Syncthing keeps syncing.
- **One owner for mutable state.** `status.Engine` owns all status state in one goroutine. Every other part only receives immutable `model.Snapshot` values, so nothing needs locks around the status data.
- **Threads.** On Windows, the tray runs a native Win32 message loop on the locked main OS thread. On macOS and Linux, `fyne.io/systray` calls back on its own thread, and the app passes work over channels.

## Components

| Package | Role |
|---|---|
| `internal/brand` | Every user-visible name, identifier, version and the disclaimer. A rename touches one file. |
| `internal/model` | Shared data types: `Snapshot`, `State`, `Peer`, `Folder`, `ActivityItem`, `Candidate`, pending devices and folders. |
| `internal/stclient` | Syncthing REST client with typed errors (unreachable, unauthorized, bad response), config discovery, `config.xml` reading, and the GUI exposure audit. |
| `internal/status` | The engine, the state machine, text formatters, the transport classifier, the event-to-activity map, notification transitions and redacted diagnostics. |
| `internal/stinstall` | Finds an existing Syncthing, or downloads, verifies, extracts, generates and starts the pinned one. Also applies transport profiles and checks versions. `pins.go` holds the SHA-256 pins. |
| `internal/autostart` | Start-at-login entries for the tray and for Syncthing: HKCU Run, LaunchAgents, systemd user units and XDG autostart. It also detects entries that other tools created. |
| `internal/tailnet` | Finds the `tailscale` CLI, parses its status, and filters pairing candidates. |
| `internal/deviceid` | Computes a Syncthing device ID from a certificate. |
| `internal/pairing` | The identity probe, the `Decide` policy, pair/accept/decline, folder sharing and the pending-request watcher. |
| `internal/icon` | Rasterises the state ring icon, and encodes it to PNG and ICO. `cmd/genicons` writes `assets/icons`. |
| `internal/tray`, `internal/notify` | The tray icon, tooltip, menu and notifications for each OS. |
| `internal/ui` | The loopback dashboard server (auth, SSE, actions), demo mode and the embedded web app (`web/`). |
| `internal/uihost` | Windows: the WebView2 popup child and its placement at the taskbar. macOS and Linux: the browser launch file. |
| `internal/glass` | A pure-Go port of the prototype's Liquid Glass optics, plus the Windows screen capture. |
| `internal/picker` | The native folder picker (Windows shell dialog, `osascript`, `zenity` or `kdialog`). |
| `internal/install` | Per-user install, upgrade and uninstall; the Start Menu shortcut and Uninstall key; legacy tray migration; the Windows Firewall rule. |
| `internal/doctor` | The `stv2 doctor` checks and their codes. |
| `internal/update` | The daily release check. |
| `internal/prefs`, `internal/applog`, `internal/single`, `internal/osutil` | Preferences (atomic JSON), the rolling redacted log, single instance, and OS paths and helpers. |
| `internal/app` | Wires everything together: engine → tray, notifications and dashboard; command dispatch; the first-run setup. |
| `cmd/stv2` | The command line: tray, `install`, `uninstall`, `pair --list`, `doctor`, `firewall allow`, `profile`, `demo`, `version`. |

Runtime dependencies are `golang.org/x/sys`, `golang.org/x/image`, `fyne.io/systray` (macOS and Linux), `github.com/godbus/dbus/v5` (Linux), `github.com/wailsapp/go-webview2` and `github.com/go-ole/go-ole` (Windows). Everything else is the Go standard library. See [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md).

## Data flow

### Status: Syncthing → tray and dashboard

1. **Connect.** `stclient.Discover` finds the Syncthing configuration: `STHOMEDIR` or `STCONFDIR`, then `syncthing paths` (v2), then `syncthing --paths` (v1), then the OS default. It reads the GUI address and API key from `config.xml`. A wildcard address (`0.0.0.0`, `::`, `*`, empty) becomes `127.0.0.1:<port>`. With HTTPS on, only Syncthing's own `https-cert.pem` is trusted.
2. **Poll and listen.** At startup the engine reads the system status, devices, folders and folder status. Every 2 s it reads `/rest/system/connections` for peers and transfer rates, and every 30 s the pending lists. An event long-poll (`/rest/events`, 50 s, with a 3 s back-off on error) delivers folder summaries, state changes, device connections and configuration changes as they happen. Every 30 s, and after every command, it does a full check.
3. **Decide the state.** The first matching rule wins:
   - Syncthing unreachable → Down
   - API key rejected → Unauthorized
   - unexpected response → Error
   - no folders → In sync
   - every folder paused → Paused
   - a folder error → Error
   - no peer connected → Disconnected, even while data is still needed
   - syncing or data needed → Syncing
   - scanning → Scanning
   - otherwise → In sync
4. **Publish.** Each change produces a new `Snapshot`, with the lines of text, rates, percent, time left, peers with their transport (Tailscale, local network, relay or direct), recent activity and notices. The tray updates its icon, tooltip and menu from it. The dashboard receives it over Server-Sent Events. Notifications fire on state transitions.

### Commands: dashboard or menu → Syncthing

The menu and the dashboard send the same named actions: rescan, pause, resume, restart, start Syncthing, open folder, pair, accept, share and so on. The app runs them against the REST API and then triggers a refresh. The dashboard's JavaScript never sees the Syncthing API key. It talks only to the loopback server, which calls Syncthing itself.

### Pairing: tailnet → Syncthing configuration

Discovery reads `tailscale status --json`, keeps online Windows, macOS and Linux peers that are not shared-in or tagged, and probes `<100.x>:22000` with a TLS handshake. The handshake is aborted as soon as the peer's certificate is read. The certificate gives the peer's device ID without the peer ever recording a request. Pairing writes the peer as a device with its tailnet addresses (`PUT /rest/config/devices/{id}`, an upsert). On the other computer, the watcher sees the pending device, verifies it (tailnet address, known node, probe matches the ID) and asks the user. See [pairing.md](pairing.md) and [security.md](security.md).

### Dashboard: tray → host → page

On Windows, showing the dashboard works like this:

1. Compute the popup rectangle on the monitor under the cursor, at the taskbar edge.
2. Capture the desktop behind it.
3. Render the glass backdrop.
4. Tell the prewarmed `stv2 ui-host` child to show itself at that rectangle.
5. The page fetches the backdrop and plays its entrance animation.

If WebView2 is missing, the child exits with code 3, and the tray switches to the browser path for the rest of the session.

On macOS and Linux, the tray writes a small HTML file (mode 0600, in a 0700 directory under `$XDG_RUNTIME_DIR` or the user cache directory). The file submits a single-use token to `/login`. The tray opens it in the default browser and deletes it after 30 s. The page's layout, cards and animations are the same on every OS. Only the material behind them differs (see [design.md](design.md)).

## Files and directories

| | Windows | macOS | Linux |
|---|---|---|---|
| Program | `%LOCALAPPDATA%\Programs\SyncThingV2\stv2.exe` | `SyncThing V2.app` | `~/.local/share/syncthing-v2/bin/stv2` or `/usr/bin/stv2` |
| Data (prefs, instance, logs) | `%LOCALAPPDATA%\SyncThingV2` | `~/Library/Application Support/SyncThingV2`, logs in `~/Library/Logs/SyncThingV2` | `~/.local/share/syncthing-v2` |
| Managed Syncthing | `%LOCALAPPDATA%\Programs\SyncThingV2\syncthing` | `~/Library/Application Support/SyncThingV2/syncthing` | `~/.local/share/syncthing-v2/syncthing` |

The log (`stv2.log`, rolled at 1 MiB, with one previous file kept) never contains the Syncthing API key or `X-API-Key` headers. `instance.json` (mode 0600) holds the dashboard server's port, the tray's PID and the control token. A second `stv2` launch uses that token to ask the running tray to show its dashboard or quit.
