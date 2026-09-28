# Installing on Windows

SyncThing V2 supports Windows 10 version 1809 or later and Windows 11, on x64. Arm64 Windows runs the x64 build through its built-in emulation. The install is per user and never asks for administrator rights. The only step that does is the optional firewall rule below.

Before you start, install [Tailscale](https://tailscale.com/download/windows) and sign in. See [tailscale-setup.md](tailscale-setup.md).

## Option 1: the PowerShell one-liner (recommended)

Open **Windows PowerShell** (not as administrator) and paste:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; irm https://github.com/Neutx/syncthing-v2/releases/latest/download/install.ps1 | iex
```

**Why the first part is needed.** Windows PowerShell 5.1, the version built into Windows 10 and 11, may not offer TLS 1.2 when it connects. GitHub accepts only TLS 1.2 or newer. The value `3072` is TLS 1.2, and `-bor` adds it to the protocols already enabled without removing any. Without that prefix, `irm` can fail with "The underlying connection was closed". The change applies only to the current PowerShell window.

The script (`install.ps1`, published with each release) then:

1. downloads `SyncThingV2-Setup-<version>-windows-x64.exe` and `SHA256SUMS.txt` into a new folder under `%TEMP%`
2. checks the exe's SHA-256 against `SHA256SUMS.txt`, and stops and deletes the download if it does not match
3. removes the downloaded-from-internet mark (`Unblock-File`) and runs `stv2.exe install --yes`
4. deletes the temporary folder

It works under any execution policy, because nothing is saved as a script file. It never elevates. To install a specific release, set `$env:STV2_VERSION = '1.0.0'` in the same window before running the one-liner.

## Option 2: the setup exe

1. Download `SyncThingV2-Setup-<version>-windows-x64.exe` from the [latest release](https://github.com/Neutx/syncthing-v2/releases/latest). If you like, [verify it](../README.md#verifying-downloads).
2. Double-click it. Release 1.0 is not code-signed, so **Microsoft Defender SmartScreen** may show "Windows protected your PC". Click **More info**, check that the file name is right, then click **Run anyway**.
3. Answer **Yes** to "Install SyncThing V2 for *your user name*?".

The exe is the whole program. Installing copies it into place. You can delete the downloaded file afterwards.

## What happens next

SyncThing V2 starts with its dashboard open and runs the first-time setup:

- **Syncthing.** A running or installed Syncthing is found and used as it is (see [migrating-from-manual-setup.md](migrating-from-manual-setup.md)). If there is none, SyncThing V2 downloads the pinned upstream release, checks its SHA-256, and sets it up with the tailnet-only transport profile. It also starts Syncthing at login. The Syncthing control panel stays on `127.0.0.1`.
- **Tailscale.** If Tailscale is missing, the dashboard shows a notice with an **Install with winget** button. That button runs `winget install -e --id Tailscale.Tailscale` in a visible window, and only when you click it.
- **Older tray app.** If an older "Syncthing Tray" app is running, you are asked once whether to replace it.

Then open **Pair devices** from the dashboard or the tray menu. [pairing.md](pairing.md) walks through it.

The tray icon lives in the notification area. If Windows hides it under the **^** arrow, drag it onto the taskbar to keep it visible. Left-click the icon, or click a notification, to open the dashboard. Right-click it for the menu.

## What is installed where

| Item | Location |
|---|---|
| Program | `%LOCALAPPDATA%\Programs\SyncThingV2\stv2.exe` |
| Start Menu shortcut | `%APPDATA%\Microsoft\Windows\Start Menu\Programs\SyncThing V2.lnk` |
| Start at login | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, value `SyncThingV2` = `"…\stv2.exe" --background` |
| Apps & features entry | `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\SyncThingV2` |
| Settings, logs, WebView2 data | `%LOCALAPPDATA%\SyncThingV2\` (`prefs.json`, `logs\stv2.log`, `instance.json`, `WebView2\`) |
| Syncthing, only if SyncThing V2 downloaded it | `%LOCALAPPDATA%\Programs\SyncThingV2\syncthing\syncthing.exe`, with upstream's `LICENSE.txt`, `README.txt` and `AUTHORS.txt` |
| Syncthing start at login, only if SyncThing V2 manages it | Run value `SyncThingV2-Syncthing` = `"…\syncthing.exe" serve --no-console --no-browser` |

Syncthing's own configuration and database stay where Syncthing keeps them, normally `%LOCALAPPDATA%\Syncthing`.

The dashboard uses the Microsoft Edge **WebView2** runtime, which ships with Windows 11 and current Windows 10. Without WebView2, the dashboard opens in your default browser and `stv2 doctor` reports [UI001](troubleshooting.md#ui001).

## Firewall

Windows Defender Firewall may ask whether Syncthing may accept connections the first time Syncthing listens. Allowing that needs administrator rights. You can skip it: pairing and sync need only one of the two computers to accept incoming connections.

To let other devices on your tailnet or local network connect to this one, run:

```powershell
& "$env:LOCALAPPDATA\Programs\SyncThingV2\stv2.exe" firewall allow
```

This opens a UAC prompt and adds one inbound rule named **SyncThing V2 - Syncthing**, for TCP and UDP. The rule applies only to the Syncthing program, only to port 22000, and only to traffic from `100.64.0.0/10` (Tailscale) and the local subnet. `stv2 doctor` reports [FW001](troubleshooting.md#fw001) while the rule is missing.

## Upgrading

Run the one-liner again, or run a newer setup exe. The installer asks the running copy to exit, replaces `stv2.exe` (the previous exe is kept as `stv2.exe.old` until the next start) and starts the new version. Settings and pairings are kept.

SyncThing V2 checks GitHub once a day for a newer release and tells you about it. It never downloads updates by itself. You can turn the check off in **Settings**.

Syncthing upgrades itself through its own auto-upgrade, whether SyncThing V2 downloaded it or not.

## Uninstalling

Use **Settings → Apps → Installed apps → SyncThing V2 → Uninstall**, or run:

```powershell
& "$env:LOCALAPPDATA\Programs\SyncThingV2\stv2.exe" uninstall
```

Your synced folders and Syncthing's settings are always kept. [uninstall.md](uninstall.md) lists exactly what is removed.
