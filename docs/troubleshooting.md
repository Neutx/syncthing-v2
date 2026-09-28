# Troubleshooting

Start with **`stv2 doctor`**. It checks Tailscale, Syncthing, the tailnet, the firewall and the dashboard, then lists what is wrong. Each finding has a code with a section on this page.

| OS | How to run it |
|---|---|
| Windows | `& "$env:LOCALAPPDATA\Programs\SyncThingV2\stv2.exe" doctor` |
| macOS | `"$HOME/Applications/SyncThing V2.app/Contents/MacOS/stv2" doctor` (or `/Applications/…`) |
| Linux | `stv2 doctor` |

Add `--json` for machine-readable output. The exit code is 1 when there is a **high** finding, and 0 otherwise. Doctor only reads. It never changes Syncthing, Tailscale or the firewall.

Severities:

- **high:** syncing or pairing cannot work
- **warn:** something is degraded
- **info:** worth knowing

## TS001

**Tailscale CLI not found** (high)

SyncThing V2 could not find the `tailscale` command-line tool, so it cannot list or verify your devices. It looks on your `PATH` and in the usual install locations:

| OS | Locations checked |
|---|---|
| Windows | `%ProgramFiles%\Tailscale\tailscale.exe` |
| macOS | `/Applications/Tailscale.app/Contents/MacOS/Tailscale`, `/opt/homebrew/bin/tailscale`, `/usr/local/bin/tailscale` |
| Linux | `/usr/bin/tailscale`, `/usr/local/bin/tailscale`, `/snap/bin/tailscale` |

**Fix:** install Tailscale from [tailscale.com/download](https://tailscale.com/download) and sign in. On Windows, the dashboard's **Install with winget** button does this for you. SyncThing V2 looks for the tool again on every check, so no restart is needed. See [tailscale-setup.md](tailscale-setup.md).

## TS002

**Tailscale is not connected** (high)

Tailscale is installed, but its state is not "Running": it is logged out, stopped, or still starting. It is also reported when the Tailscale service did not answer.

**Fix:** open Tailscale and sign in, or on Linux run `sudo tailscale up`. Check that `tailscale status` lists your other devices. On Windows and Linux, make sure the Tailscale service is running.

## ST001

**Syncthing binary not found** (high)

No Syncthing program was found: not running, not on `PATH`, not in the known locations, and not in SyncThing V2's managed folder.

**Fix:** open SyncThing V2 and choose **Start Syncthing** from the tray menu or the dashboard. When Syncthing is missing, this downloads the pinned, checksum-verified release and sets it up. You can also install Syncthing yourself, from [syncthing.net](https://syncthing.net/downloads/), your package manager, Homebrew, scoop or winget. SyncThing V2 then uses it as it is.

## ST002

**Syncthing is not responding** (high)

Syncthing is installed but does not answer on its GUI address (normally `127.0.0.1:8384`). The configuration may also be missing or unreadable, or Syncthing may have returned something unexpected. The tray shows "Not running" with a grey icon.

**Fix:** choose **Start Syncthing** in the tray menu (it appears only in this state). If Syncthing starts and stops again, check its log:

- in the web UI, under **Actions → Logs**
- on macOS with a managed Syncthing, in `~/Library/Logs/SyncThingV2/syncthing.log`
- on Linux, with `journalctl --user -u stv2-syncthing.service` (or `syncthing.service`)

If you run Syncthing with a non-default home (`--home`), start SyncThing V2 with the same `STHOMEDIR` environment variable so both use the same configuration.

## ST003

**Syncthing rejected the API key** (high)

Syncthing answered with 401 or 403. The API key that SyncThing V2 read from `config.xml` is not the one Syncthing is using. This usually happens when the configuration was regenerated or edited while Syncthing was running, or when SyncThing V2 is reading a different configuration from the one Syncthing uses (another `--home`). The tray shows "API key rejected" with an X icon.

**Fix:** restart Syncthing so it reloads its configuration. Use its web UI (**Actions → Restart**), or stop and start it. If you use a custom home directory, set `STHOMEDIR` for SyncThing V2 as well. SyncThing V2 reconnects by itself every 30 seconds.

## ST004

**Syncthing is older than the minimum supported version** (warn)

SyncThing V2 needs Syncthing **v1.27.0** or newer.

**Fix:** upgrade Syncthing. If Syncthing's auto-upgrade is on, use **Actions → Settings → General → Automatic upgrades** in its web UI, or wait for its next check. Otherwise use your package manager, or download it from [syncthing.net](https://syncthing.net/downloads/).

## ST005

**Syncthing does not listen on the default sync port 22000** (info)

This Syncthing listens for sync connections on another port. SyncThing V2's pairing discovery only probes port 22000, so other computers see this one as "Install SyncThing V2 on this device (or it uses a non-default port)".

**Fix:** pair from this computer, which can still reach the others, or pair by hand. Both are described in [pairing.md → Non-default ports](pairing.md#non-default-ports). To go back to the default, set **Settings → Connections → Sync Protocol Listen Addresses** to `default` in Syncthing's web UI, then restart Syncthing.

## SEC001

**Syncthing control panel reachable from the network without protection** (high)

Syncthing's web UI and REST API listen on an address other computers can reach (such as `0.0.0.0:8384`) with no GUI password. Or `insecureAdminAccess` is turned on. Anyone who can reach that port can then control your Syncthing and read your files.

**Fix:** click **Restrict** in the dashboard notice "Your Syncthing control panel is reachable from other devices without a password. Restrict it to this computer?". That sets the GUI address to `127.0.0.1:<port>` and turns `insecureAdminAccess` off. Or, in Syncthing's web UI, set **Settings → GUI → GUI Listen Address** to `127.0.0.1:8384` and set a GUI user and password. **Don't ask again** hides the notice for good, but doctor keeps reporting it.

## NET001

**Syncthing on a tailnet device is not reachable on port 22000** (warn)

Doctor probed one of your online devices at `<100.x address>:22000` and got no Syncthing answer. The message says whether the connection was refused, timed out, or reached something that is not Syncthing. It lists one finding per device.

**Fix:** on that device, check each of these:

1. SyncThing V2 or Syncthing is installed and running.
2. Your tailnet policy allows port 22000 (see [tailscale-setup.md](tailscale-setup.md#3-let-port-22000-through-the-tailnet-policy)).
3. Its firewall allows Syncthing (see [FW001](#fw001) for Windows, and [firewalls on macOS and Linux](#firewalls-on-macos-and-linux)).
4. Tailscale's Shields Up is off there.

Only one of two computers needs to accept connections for sync to work, so a single NET001 is often harmless once the pair is connected.

## FW001

**Windows Firewall rule for Syncthing is missing** (info, Windows)

The inbound rule **SyncThing V2 - Syncthing** does not exist, so Windows may block other devices that try to connect to Syncthing on this computer.

**Fix:** run the command below and accept the UAC prompt:

```powershell
& "$env:LOCALAPPDATA\Programs\SyncThingV2\stv2.exe" firewall allow
```

It adds a rule for the Syncthing program only, on port 22000 (TCP and UDP), for traffic from `100.64.0.0/10` and the local subnet. Pairing usually works without the rule, because only one side needs to accept connections. Uninstalling SyncThing V2 does not remove the rule, because that needs administrator rights. Remove it in **Windows Defender Firewall with Advanced Security** if you no longer want it.

## UI001

**WebView2 runtime missing** (warn, Windows)

The Microsoft Edge WebView2 runtime is not installed, so the glass dashboard popup cannot start. SyncThing V2 then opens the dashboard in your default browser for the rest of the session.

**Fix:** install the Evergreen WebView2 runtime from [developer.microsoft.com/microsoft-edge/webview2](https://developer.microsoft.com/microsoft-edge/webview2/), then exit and restart SyncThing V2.

## UI002

**No tray host (StatusNotifierWatcher) on the session bus** (warn, Linux)

Your desktop has no StatusNotifierItem host, so the tray icon cannot appear. SyncThing V2 then sends a notification and opens its dashboard in the browser instead.

**Fix:** on GNOME, install and enable the **AppIndicator and KStatusNotifierItem Support** extension (Ubuntu's "Ubuntu AppIndicators" is the same thing), then log out and back in. See [install-linux.md](install-linux.md#gnome-the-tray-icon-needs-an-extension). On other desktops, add a system tray or status notifier applet to the panel.

## UPD001

**A newer SyncThing V2 release exists** (info)

A newer release is on GitHub. The message links to it.

**Fix:** run the one-liner for your OS again, or download the new release. Settings and pairings are kept. The tray menu also shows **Update available (vX.Y.Z)…**. To stop checking, turn off **Settings → Check for updates**.

## Windows SmartScreen: "Windows protected your PC"

Release 1.0 is not code-signed. SmartScreen warns about new unsigned programs until they build up a reputation. Click **More info**, check that the app is `SyncThingV2-Setup-<version>-windows-x64.exe`, then click **Run anyway**. The PowerShell one-liner avoids the prompt: it verifies the checksum and then runs the program directly. If Microsoft Defender flags or deletes the file, please [open an issue](https://github.com/Neutx/syncthing-v2/issues) and include the version. Every release is scanned with VirusTotal before it is published, and any detection is submitted to Microsoft as a false positive.

## macOS Gatekeeper: "cannot be opened" or "Apple could not verify"

Release 1.0 is not notarized. On macOS 15 and later, close the warning, then open **System Settings → Privacy & Security** and click **Open Anyway** next to the message about SyncThing V2. On macOS 12 to 14, Control-click the app and choose **Open**. The Terminal one-liner avoids the prompt. See [install-macos.md](install-macos.md).

## No tray icon

- **Windows:** the icon may be hidden under the **^** arrow in the notification area. Drag it onto the taskbar. If SyncThing V2 is not running at all, start it from the Start Menu.
- **macOS:** on a crowded menu bar, macOS hides items behind the notch or when there is no room. Close other menu bar apps or use a menu bar manager.
- **Linux (GNOME):** see [UI002](#ui002). Run `stv2 doctor` to confirm.

In every case, starting SyncThing V2 again while it is already running opens its dashboard.

## Firewalls on macOS and Linux

`stv2 firewall allow` exists only on Windows.

- **macOS:** the application firewall is off by default. When it is on, allow `syncthing` when macOS asks. The prompt can return after Syncthing upgrades itself. Check it under **System Settings → Network → Firewall → Options**.
- **Linux with ufw:** `sudo ufw allow in on tailscale0 to any port 22000`. With firewalld, add port 22000/tcp and 22000/udp to the zone of the `tailscale0` interface.

## Logs and diagnostics

SyncThing V2's log is rolled at 1 MiB, with one previous file kept as `stv2.log.1`:

| OS | Log file |
|---|---|
| Windows | `%LOCALAPPDATA%\SyncThingV2\logs\stv2.log` |
| macOS | `~/Library/Logs/SyncThingV2/stv2.log` |
| Linux | `~/.local/share/syncthing-v2/logs/stv2.log` |

**Settings → Open logs folder** in the dashboard opens that folder. The log never contains the Syncthing API key.

**Settings → Copy diagnostics** puts a redacted report on the clipboard: state, folder and peer summaries, versions and transport. It has no API key, device IDs are cut to 7 characters, and there are no file paths. Paste it into a bug report.

Syncthing's own log is in its web UI (**Actions → Logs**).

## Starting over

To remove SyncThing V2 but keep everything Syncthing knows, see [uninstall.md](uninstall.md). Installing again later adopts the same Syncthing, with the same devices and folders.
