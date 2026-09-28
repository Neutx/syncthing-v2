# Changelog

All notable changes to SyncThing V2 are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - Unreleased

The first public release. SyncThing V2 replaces the Windows-only "Syncthing Tray" prototype, a C# WinForms tray with a "Liquid Glass" dashboard. It keeps every prototype feature, and runs on Windows, macOS and Linux.

### Added

- **Pairing over Tailscale.** SyncThing V2 discovers your online computers on the tailnet, reads their Syncthing device IDs with a side-effect-free TLS probe on port 22000, and pairs them with a click on each machine. Pairing requests are shown only after three checks: the request comes from a tailnet address, that address belongs to a known tailnet device, and the device there holds the requesting ID. Other owners' devices are flagged, and shared-in and tagged devices are excluded.
- **Folder sharing** from the dashboard. You can share an existing folder or pick a new one. Offers from paired devices are accepted into a safe, collision-free folder under `~/Sync`, or into a location you choose.
- **Syncthing bootstrap.** An existing Syncthing is adopted untouched. When none is found, the pinned upstream release is downloaded and checked against SHA-256 pins, which are generated from Syncthing's GPG-signed checksums. It is then set up with the tailnet-only transport profile and started at login. Syncthing's own auto-upgrade stays on.
- **Transport profiles:** `tailnet` (the default for new setups) and `hybrid` (upstream's defaults), switchable in Settings or with `stv2 profile`.
- **Self-installing, per-user packages:**
  - Windows: a setup exe, a Start Menu shortcut and an Apps & features entry
  - macOS: a `.dmg` and a `.tar.gz`
  - Linux: a `.deb` and `.tar.gz` for x64 and arm64
  - one-line installers that verify `SHA256SUMS.txt` before running anything
  - `stv2 uninstall`, which never touches Syncthing's configuration or your data
- **`stv2 doctor`** with stable finding codes (TS001–UPD001), each documented in `docs/troubleshooting.md`. Also `stv2 pair --list`, `stv2 firewall allow` (a scoped Windows Firewall rule through UAC) and `stv2 demo`.
- **Settings view:** start at login for SyncThing V2 and for Syncthing, transport profile, notifications, update check, "Copy diagnostics" and "Open logs folder".
- **One-time notices:** for a Syncthing control panel exposed on the network (with a one-click fix), for the legacy tray app, for missing Tailscale (with **Install with winget** on Windows), and for new releases. The update check is one request to GitHub a day and can be turned off. Updates are never downloaded automatically.
- **Notifications** for pairing requests, folder offers, a rejected API key and available updates, in addition to the prototype's state notifications. They can be turned off.
- **macOS and Linux support.** The tray is a menu bar or StatusNotifierItem icon, and the dashboard opens in the default browser through a single-use login link.
- **A rolling application log** (1 MiB × 2) that never contains the Syncthing API key.
- **Reproducible release builds** with build provenance attestations for every asset.

### Changed

- The tray autostart is a per-user registry Run value (Windows), a LaunchAgent (macOS) or an XDG autostart entry (Linux). It replaces the prototype's Startup-folder shortcut.
- Syncthing's own start-at-login can now be switched from the tray menu when SyncThing V2 owns that entry. An entry configured elsewhere is detected and shown read-only.
- The user-facing text is generalised from "the other laptop" to any number of devices:
  - "All devices hold the same files."
  - "No paired device is reachable right now."
  - "Waiting for a device"
- The single-instance mutex is now `Local\SyncThingV2.Tray`, so SyncThing V2 and the prototype can run side by side during migration. Launching SyncThing V2 a second time opens the running copy's dashboard.
- The Windows install detects the prototype tray and offers, once, to stop it and disable its startup shortcut. The shortcut is renamed to `Syncthing Tray.lnk.disabled`, which can be reversed.

### Fixed

These are deliberate behaviour fixes relative to the prototype:

- **Disconnected devices no longer show as "Syncing".** When no paired device is connected, the state is now "Disconnected" even while data is still needed. The folder line then shows the percent and the amount left, and the disconnect notification fires.
- **Not every failure means "Syncthing not running" any more.** A rejected API key (401/403) shows as "API key rejected", with its own icon and notification. An unexpected response shows as an error. Only an unreachable Syncthing counts as Down. **Start Syncthing** is offered only in the Down state and is debounced, so it can no longer start a second Syncthing.
- **Every peer and folder is shown.**
  - The peer line lists every connected device with its transport, and the connection card shows "+N more".
  - Folders are tracked individually and in aggregate.
  - **Open Sync Folder** becomes a submenu when there are several folders.
- **Transport detection uses real address ranges.**
  - Tailscale means `100.64.0.0/10` or `fd7a:115c:a1e0::/48`, not any address that starts with `100.`.
  - Relay connections are recognised first, even over Tailscale.
  - `172.16.0.0/12`, `169.254.0.0/16` and IPv6 ULA and link-local addresses count as local network.
- **Less load on Syncthing.** Folder status now comes from `FolderSummary` events, with a 30 s reconciliation. The prototype called the expensive `/rest/db/status` for every folder every 2 s.
- **Thread safety.** All status state is owned by one goroutine, and the tray, notifications and dashboard receive immutable snapshots. The prototype shared counters and event IDs between threads without synchronisation.
- **The error detail is now used.** "Copy diagnostics" puts a redacted report on the clipboard, with no API key, 7-character device IDs and no paths.
- **High-DPI displays.** The Windows build is per-monitor DPI aware (PerMonitorV2 manifest). The dashboard and the backdrop capture scale with the monitor under the cursor.
- **Window placement.** The dashboard opens on the monitor under the cursor, at whichever edge the taskbar is on. The prototype always used the primary screen's bottom-right corner.
- **Syncthing configurations that use HTTPS, or a non-default home** (`STHOMEDIR`/`STCONFDIR`, `syncthing paths`), are supported.
- **Tray tooltip.** It uses the native 127-character tooltip, with no reflection workaround.
- **Design constants.** The dashboard design documentation now matches the shipped code (blur radius 5, saturation 2.0, window radius 40 and others). See `docs/design.md`.
- **Removed the hover scale on buttons.** Hover now changes only fill and rim brightness, and a CI lint forbids any hover-raise effect.

### Security

- The dashboard is served on `127.0.0.1` only, with these protections:
  - Host and Origin checks
  - a single-use 30-second launch token
  - a `SameSite=Strict`, `HttpOnly` session cookie
  - a required header on POSTs
  - a strict Content-Security-Policy
  - no CORS headers
- The Syncthing API key never reaches the browser, the log or diagnostics.
- Downloaded archives are extracted with zip-slip and tar-slip protection, and remote folder labels are sanitised before they become paths.
- Nothing is accepted automatically: no device and no folder.

[Unreleased]: https://github.com/Neutx/syncthing-v2/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/Neutx/syncthing-v2/releases/tag/v1.0.0
