# Installing on Linux

> **Preview.** The Linux build is built and tested in CI, but nobody has yet confirmed the tray on real desktops. Please [report](https://github.com/Neutx/syncthing-v2/issues) how it works on yours.

SyncThing V2 runs on x64 and arm64 Linux, on glibc or musl distributions (the binary is static), under X11 or Wayland. Everything installs for your user account, and nothing needs `sudo` except installing the `.deb` itself.

Before you start, install [Tailscale](https://tailscale.com/download/linux) and sign in. See [tailscale-setup.md](tailscale-setup.md).

On Linux, SyncThing V2 is a tray (StatusNotifierItem) icon, and its dashboard opens in your **default web browser**.

## Option 1: the shell one-liner (recommended)

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh
```

The script (`install.sh`, published with each release):

1. picks `SyncThingV2-<version>-linux-x64.tar.gz` or `…-linux-arm64.tar.gz` for your machine
2. downloads it and `SHA256SUMS.txt` into a private temporary folder, which is removed afterwards
3. checks the SHA-256 with `sha256sum -c`, and stops without installing anything if it does not match
4. runs `./stv2 install --yes` from the archive, which installs SyncThing V2 for you and starts it

It never runs `sudo` and refuses to run as root; `stv2` itself also refuses to install or run as root unless `STV2_ALLOW_ROOT=1` is set (the script's `--allow-root` sets it). It needs `curl` or `wget`, `tar` and `sha256sum`. To install a specific release, run `curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | STV2_VERSION=1.0.0 sh`.

## Option 2: the .deb package (Debian, Ubuntu and derivatives)

Download `syncthing-v2_<version>_amd64.deb` (or `_arm64.deb`) from the [latest release](https://github.com/Neutx/syncthing-v2/releases/latest). If you like, [verify it](../README.md#verifying-downloads). Then run:

```sh
sudo apt install ./syncthing-v2_1.0.0_amd64.deb
```

The package installs `/usr/bin/stv2`, the menu entry `/usr/share/applications/stv2.desktop` and icons in the hicolor theme. It has no dependencies. Start **SyncThing V2** from your application menu. The first launch sets up Syncthing for your user. To also start SyncThing V2 at login, turn on **Start SyncThing V2 on login** in its menu or in **Settings**, or run `stv2 install --yes` once.

## Option 3: the tarball

Download `SyncThingV2-<version>-linux-x64.tar.gz` (or `-linux-arm64`), verify it, then:

```sh
mkdir stv2-install && tar -xzf SyncThingV2-1.0.0-linux-x64.tar.gz -C stv2-install
./stv2-install/stv2 install --yes
```

The archive contains `stv2`, `LICENSE` and `THIRD_PARTY_NOTICES.md`. You can delete the extracted folder afterwards.

## What happens next

SyncThing V2 opens its dashboard in your browser and runs the first-time setup:

- **Syncthing.** A running Syncthing, or one installed from your distribution, Snap (run through `/snap/bin/syncthing`), Homebrew on Linux, Nix, `/usr/local/bin` or `~/.local/bin`, is found and used as it is. If there is none, SyncThing V2 downloads the pinned upstream release, checks its SHA-256, and sets it up with the tailnet-only transport profile. It starts Syncthing at login (see below).
- **Tailscale.** If the `tailscale` command cannot be found in `/usr/bin`, `/usr/local/bin`, `/snap/bin` or on your `PATH`, the dashboard shows a notice.
- **Choosing a folder.** Choosing a folder (to share a new one, or where to save one that is offered to you) uses zenity or kdialog when one is installed (for example `sudo apt install zenity`). Without either, the dashboard asks you to type the folder's full path.

Then open **Pair devices**. [pairing.md](pairing.md) walks through it.

**Using it.** On most desktops, clicking the tray icon opens its menu. **Open Status Window** opens the dashboard in your browser. The browser signs in with a single-use link, so open the dashboard from the menu each time instead of bookmarking it. Notifications go through the desktop's notification service (D-Bus). Clicking one opens the dashboard.

## GNOME: the tray icon needs an extension

GNOME Shell shows no tray icons by default. Install the **AppIndicator and KStatusNotifierItem Support** extension. Ubuntu ships it preinstalled as "Ubuntu AppIndicators". On other distributions it is usually packaged as `gnome-shell-extension-appindicator`, or you can get it from [extensions.gnome.org](https://extensions.gnome.org/extension/615/appindicator-support/). Log out and back in after enabling it.

Without a tray host (SyncThing V2 waits up to 30 seconds at login for the panel to provide one), SyncThing V2 sends a notification saying so, opens its dashboard in the browser instead, and `stv2 doctor` reports [UI002](troubleshooting.md#ui002). KDE Plasma, Xfce, Cinnamon, MATE and most other desktops show the icon without extra steps.

## Starting at login

| What | How |
|---|---|
| SyncThing V2 | `~/.config/autostart/stv2.desktop` (`stv2 --background`) |
| Syncthing from your distribution (it ships `syncthing.service` as a user unit) | `systemctl --user enable syncthing.service` (it takes over from the next login) |
| Syncthing that SyncThing V2 downloaded | the user unit `~/.config/systemd/user/stv2-syncthing.service`, using upstream's restart rules |
| Either one, when `systemctl --user` is not available | `~/.config/autostart/stv2-syncthing.desktop` |

A Syncthing autostart that you set up yourself (any other user unit or autostart entry whose program is `syncthing`, such as `homebrew.syncthing.service`) is detected and left alone. The "Start Syncthing on login" toggle is then shown read-only. Check the unit with `systemctl --user status stv2-syncthing.service`, or `syncthing.service` for a distribution package.

## What is installed where

| Item | Location |
|---|---|
| Program (one-liner, tarball) | `~/.local/share/syncthing-v2/bin/stv2`, linked from `~/.local/bin/stv2` |
| Program (.deb) | `/usr/bin/stv2` |
| Menu entry and icons (one-liner, tarball) | `~/.local/share/applications/stv2.desktop`, `~/.local/share/icons/hicolor/<size>/apps/stv2.png` |
| Settings | `~/.local/share/syncthing-v2/` (`prefs.json`, `instance.json`) |
| Logs | `~/.local/share/syncthing-v2/logs/stv2.log` |
| Syncthing, only if SyncThing V2 downloaded it | `~/.local/share/syncthing-v2/syncthing/syncthing`, with upstream's `LICENSE.txt`, `README.txt` and `AUTHORS.txt` |

`~/.local/share` is `$XDG_DATA_HOME` when that is set. Syncthing's own configuration stays where Syncthing keeps it, normally `~/.local/state/syncthing` or `~/.config/syncthing`. If `~/.local/bin` is not on your `PATH`, the installer tells you. Add it to run `stv2` from a terminal.

## Firewall

With `ufw`, allow Syncthing's port on the Tailscale interface only:

```sh
sudo ufw allow in on tailscale0 to any port 22000
```

Do this whenever your firewall blocks incoming connections: pairing through SyncThing V2 needs both computers to accept connections on port 22000. The computer where you click **Accept** checks the computer where you clicked **Pair** on that port before it shows the request. If the port is blocked on the Pair side, the request never appears (see [pairing.md](pairing.md)). With firewalld, add port 22000/tcp and 22000/udp to the zone of the `tailscale0` interface.

## Upgrading

Run the one-liner again, or install a newer `.deb`. Settings and pairings are kept. SyncThing V2 checks GitHub once a day for a newer release and tells you about it, but never downloads it by itself. You can turn the check off in **Settings**. A Syncthing that SyncThing V2 downloaded keeps upgrading itself through its own auto-upgrade. A distribution Syncthing is upgraded by your package manager.

## Uninstalling

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh -s -- --uninstall
```

or `stv2 uninstall`. For the `.deb`, run `stv2 uninstall` first to remove your per-user entries, then `sudo apt remove syncthing-v2`. Your synced folders and Syncthing's settings are always kept. See [uninstall.md](uninstall.md).
