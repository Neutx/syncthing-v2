# Moving from a manual Syncthing setup

If you already run Syncthing, set up by hand, from a package manager or with an older tray app, SyncThing V2 takes it over without changing it. This page explains what it adopts, what it leaves alone, and how to go back.

## Your existing Syncthing is adopted as it is

When SyncThing V2 starts, it looks for Syncthing in this order and uses the first one it finds:

1. a running `syncthing` process (it uses that exact program)
2. `syncthing` on your `PATH`
3. the usual install locations:

   | OS | Locations |
   |---|---|
   | Windows | `%LOCALAPPDATA%\Programs\Syncthing\syncthing.exe`, scoop's shim, winget's link |
   | macOS | Homebrew (`/opt/homebrew/bin`, `/usr/local/bin`), `/Applications/Syncthing.app` |
   | Linux | `/usr/bin`, `~/.local/bin`, `/snap/bin` |

4. its own managed copy, which it downloads only if none of the above exist

It finds Syncthing's configuration the way Syncthing does: `STHOMEDIR` or `STCONFDIR` if set, then what `syncthing paths` reports, then the default location for your OS. It reads the GUI address and API key from `config.xml`, and supports HTTPS GUIs.

For an adopted Syncthing, SyncThing V2 **never changes**:

- the program
- the configuration
- the transport settings: global discovery, relays and NAT stay as they are
- the start-at-login entry

It changes something only when you confirm an action: pairing or accepting a device, sharing or accepting a folder, restricting an exposed control panel, or switching the transport profile. Your existing devices and folders appear in the dashboard straight away, and devices you paired by hand show as **Paired** in the Pair view.

### Things you may be asked once

- **Exposed control panel.** If your Syncthing web UI can be reached from other devices without a password (for example `0.0.0.0:8384` with no GUI user, or `insecureAdminAccess` on), the dashboard asks: "Your Syncthing control panel is reachable from other devices without a password. Restrict it to this computer?"
  - **Restrict** sets the GUI address to `127.0.0.1:<port>` and turns `insecureAdminAccess` off.
  - **Not now** asks again at the next start.
  - **Don't ask again** stops asking. `stv2 doctor` still reports [SEC001](troubleshooting.md#sec001).
- **Transport profile.** Nothing is asked, and your settings stay. If you want tailnet-only syncing, switch it in **Settings → Transport profile**, or run `stv2 profile tailnet`. See [tailscale-setup.md](tailscale-setup.md#6-transport-profiles).

### Start at login

If Syncthing already starts at login, SyncThing V2 recognises it and leaves it alone. It recognises:

- a Windows Startup-folder shortcut to `syncthing.exe` (such as `Startup\Syncthing.lnk`)
- another HKCU Run value that mentions Syncthing
- a `~/Library/LaunchAgents/*syncthing*.plist` (for example from `brew services`)
- an enabled `syncthing*.service` user unit

The **Start Syncthing …** toggle then reads "configured outside SyncThing V2" and cannot be changed from SyncThing V2. If there is no entry at all, you can turn the toggle on, and SyncThing V2 creates and owns an entry. It creates or removes only entries it owns.

## The older "Syncthing Tray" app (Windows)

SyncThing V2 replaces an earlier Windows prototype, the "Syncthing Tray" app (`SyncthingTray.exe`, started by `Syncthing Tray.lnk` in your Startup folder). When SyncThing V2 finds that shortcut, or the app running, it asks **once**, either in the installer or as a dashboard notice:

> An older Syncthing tray app is running. Replace it with SyncThing V2?

- **Replace:** SyncThing V2 stops `SyncthingTray.exe` and renames `Syncthing Tray.lnk` in your Startup folder (`%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup`) to **`Syncthing Tray.lnk.disabled`**. Nothing is deleted. Your Syncthing and its own startup shortcut (`Syncthing.lnk`, if you have one) are not touched.
- **No:** nothing changes, and SyncThing V2 does not ask again. The two apps can run side by side. You will see two tray icons.

### Going back

To use the older tray app again:

1. Open the Startup folder: press **Win+R** and run `shell:startup`.
2. Rename `Syncthing Tray.lnk.disabled` back to `Syncthing Tray.lnk`.
3. Double-click it to start the old tray now, or sign out and back in.

To stop SyncThing V2 from starting as well, turn off **Start SyncThing V2 with Windows** in its menu, or [uninstall it](uninstall.md). Uninstalling SyncThing V2 does not restore the shortcut, because you may want neither app.

## Going back to a manual setup

SyncThing V2 keeps no state that Syncthing depends on:

- Devices it paired are ordinary Syncthing devices with `tcp://` and `quic://` tailnet addresses, plus `dynamic` after the first connection.
- Folders it shared or accepted are ordinary Syncthing folders.

Uninstall SyncThing V2 ([uninstall.md](uninstall.md)), and Syncthing carries on exactly as configured. If SyncThing V2 had downloaded Syncthing for you, that copy is kept by default and keeps running at login. You can later replace it with a Syncthing from syncthing.net or your package manager, using the same home directory.
