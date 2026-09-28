# Installing on macOS

> **Preview.** The macOS build is built and tested in CI, but nobody has yet confirmed it on real hardware. Please [report](https://github.com/Neutx/syncthing-v2/issues) how it works for you.

SyncThing V2 supports macOS 12 (Monterey) or later on Apple silicon and Intel. It is one universal app. Everything installs for your user account, and nothing needs `sudo`.

Before you start, install [Tailscale](https://tailscale.com/download/mac) and sign in. See [tailscale-setup.md](tailscale-setup.md).

On macOS, SyncThing V2 is a menu bar item. Its dashboard opens in your **default web browser**. The glass popup is Windows only.

## Option 1: the Terminal one-liner (recommended)

Open **Terminal** and paste:

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh
```

The script (`install.sh`, published with each release):

1. downloads `SyncThingV2-<version>-macos-universal.tar.gz` and `SHA256SUMS.txt` into a private temporary folder
2. checks the SHA-256 with `shasum -a 256 -c`, and stops without installing anything if it does not match
3. moves **SyncThing V2.app** to `~/Applications`. An older copy there is asked to quit (`stv2 --quit`) and then replaced.
4. runs `stv2 install --yes`, which turns on **Open at Login** and starts the app

This is the recommended path because files that `curl` downloads are not quarantined, so Gatekeeper does not block the unsigned app. The script refuses to run as root. To install a specific release, run `curl -fsSL … | STV2_VERSION=1.0.0 sh`.

## Option 2: the disk image

1. Download `SyncThingV2-<version>-macos-universal.dmg` from the [latest release](https://github.com/Neutx/syncthing-v2/releases/latest). If you like, [verify it](../README.md#verifying-downloads).
2. Open it and drag **SyncThing V2** to **Applications**. Do not run it from the disk image: it will not set itself to open at login from there, because the image goes away when ejected.
3. Open SyncThing V2 from Applications. Release 1.0 is not notarized, so macOS blocks the first launch:
   - **macOS 15 (Sequoia) and later:** close the warning, open **System Settings → Privacy & Security**, scroll to the message about "SyncThing V2" and click **Open Anyway**. Then confirm with your password or Touch ID. Right-click → Open no longer skips the check on these versions.
   - **macOS 12 to 14:** Control-click the app in Finder, choose **Open**, then click **Open** in the dialog.
4. In the SyncThing V2 menu, turn on **Start SyncThing V2 at login**. You can also turn it on in **Settings**, or run `"/Applications/SyncThing V2.app/Contents/MacOS/stv2" install --yes` once in Terminal.

## What happens next

On first launch, SyncThing V2 opens its dashboard in your browser and runs the first-time setup:

- **Syncthing.** A running Syncthing, or one installed with Homebrew or as `Syncthing.app`, is found and used as it is. If there is none, SyncThing V2 downloads the pinned upstream release, checks its SHA-256, and sets it up with the tailnet-only transport profile. It starts Syncthing at login through a LaunchAgent.
- **Tailscale.** If the Tailscale command-line tool cannot be found, the dashboard shows a notice. SyncThing V2 looks for it in the Mac App Store app (`/Applications/Tailscale.app`) and in Homebrew's locations.

Then open **Pair devices**. [pairing.md](pairing.md) walks through it.

**Using it.** Clicking the menu bar icon opens the menu, as is usual on macOS. **Open Status Window** opens the dashboard in your browser. The browser tab signs in with a single-use link, so open the dashboard from the menu each time instead of bookmarking it. Notifications appear through macOS's built-in AppleScript notifications (they may be labelled "Script Editor"). Clicking one does not open the dashboard.

## What is installed where

| Item | Location |
|---|---|
| App | `~/Applications/SyncThing V2.app` (one-liner) or `/Applications/SyncThing V2.app` (disk image) |
| Open at Login | `~/Library/LaunchAgents/io.github.neutx.syncthingv2.plist` |
| Settings | `~/Library/Application Support/SyncThingV2/` (`prefs.json`, `instance.json`) |
| Logs | `~/Library/Logs/SyncThingV2/stv2.log` |
| Syncthing, only if SyncThing V2 downloaded it | `~/Library/Application Support/SyncThingV2/syncthing/syncthing`, with upstream's `LICENSE.txt`, `README.txt` and `AUTHORS.txt` |
| Syncthing start at login, only if SyncThing V2 manages it | `~/Library/LaunchAgents/io.github.neutx.syncthingv2.syncthing.plist`, logging to `~/Library/Logs/SyncThingV2/syncthing.log` |

Syncthing's own configuration and database stay in `~/Library/Application Support/Syncthing`. An existing Syncthing LaunchAgent (for example from Homebrew's `brew services`) is detected and left alone. The "Start Syncthing at login" toggle is then shown read-only.

## Application firewall

The macOS application firewall is off by default. If you turned it on, macOS asks whether `syncthing` may accept incoming connections. Click **Allow**. The question can come back after Syncthing upgrades itself, because the upgraded binary counts as a new program. Declining is not fatal: pairing and sync need only one of the two computers to accept incoming connections.

## Upgrading

Run the one-liner again, or drag a newer app from a newer disk image over the old one. Settings and pairings are kept. SyncThing V2 checks GitHub once a day for a newer release and tells you about it, but never downloads it by itself. You can turn the check off in **Settings**. Syncthing keeps upgrading itself through its own auto-upgrade.

## Uninstalling

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh -s -- --uninstall
```

or, with the app still in place:

```sh
"$HOME/Applications/SyncThing V2.app/Contents/MacOS/stv2" uninstall
```

(Use `/Applications/…` if you installed from the disk image.) This removes the login items that SyncThing V2 owns, its settings and logs, and the app when it is in `~/Applications`. An app in `/Applications` is left for you to move to the Trash. Your synced folders and Syncthing's settings are always kept. See [uninstall.md](uninstall.md).
