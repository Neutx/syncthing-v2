# Uninstalling

Uninstalling SyncThing V2 never touches:

- your synced folders and files
- Syncthing's configuration: devices, folders, keys and settings, in Syncthing's own home directory
- Syncthing's database
- a Syncthing you installed yourself: its program, its settings and its start-at-login entry are all left alone

By default it also **keeps a Syncthing that SyncThing V2 downloaded**, together with its start-at-login entry, so your folders keep syncing. To remove that Syncthing too, answer **yes** when asked, or pass `--remove-syncthing`. That stops Syncthing and deletes its program and its start-at-login entry. Syncthing's configuration, database and your files still stay.

Every uninstall first asks a running SyncThing V2 to exit. `stv2 uninstall` asks for confirmation. `stv2 uninstall --yes` does not ask, and keeps a downloaded Syncthing unless `--remove-syncthing` is also given.

## Windows

Use **Settings → Apps → Installed apps → SyncThing V2 → Uninstall** (on Windows 10, **Apps & features**). Or run:

```powershell
& "$env:LOCALAPPDATA\Programs\SyncThingV2\stv2.exe" uninstall
```

This removes:

- the start-at-login value `SyncThingV2` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
- the Start Menu shortcut **SyncThing V2**
- the Apps & features entry (`HKCU\…\Uninstall\SyncThingV2`)
- `%LOCALAPPDATA%\SyncThingV2`, which holds settings, logs and WebView2 data
- the program folder `%LOCALAPPDATA%\Programs\SyncThingV2`. A few seconds after SyncThing V2 exits, a hidden `cmd.exe` deletes it. A kept Syncthing inside it (`syncthing\`) stays.
- with `--remove-syncthing`: the downloaded Syncthing and its Run value `SyncThingV2-Syncthing`

The Windows Firewall rule **SyncThing V2 - Syncthing**, if you added it, stays, because removing it needs administrator rights. Remove it in **Windows Defender Firewall with Advanced Security → Inbound Rules**, or from an administrator PowerShell:

```powershell
netsh advfirewall firewall delete rule name="SyncThing V2 - Syncthing"
```

A legacy tray shortcut that SyncThing V2 disabled (`Syncthing Tray.lnk.disabled`) is left in place. See [migrating-from-manual-setup.md](migrating-from-manual-setup.md#going-back) to restore it.

## macOS

```sh
"$HOME/Applications/SyncThing V2.app/Contents/MacOS/stv2" uninstall
```

(Use `/Applications/…` if you installed from the disk image.) Or use the installer script:

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh -s -- --uninstall
```

This removes:

- the login item `~/Library/LaunchAgents/io.github.neutx.syncthingv2.plist`
- `~/Library/Application Support/SyncThingV2` (settings), except a kept Syncthing in its `syncthing/` folder
- `~/Library/Logs/SyncThingV2`. `syncthing.log` stays while a downloaded Syncthing is kept.
- `SyncThing V2.app`, when it is in `~/Applications`. An app in `/Applications` is left for you to move to the Trash.
- with `--remove-syncthing`: the downloaded Syncthing and `~/Library/LaunchAgents/io.github.neutx.syncthingv2.syncthing.plist`

## Linux

```sh
stv2 uninstall
```

or:

```sh
curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh -s -- --uninstall
```

This removes:

- `~/.config/autostart/stv2.desktop`
- the program `~/.local/share/syncthing-v2/bin/stv2` and the link `~/.local/bin/stv2`
- the menu entry `~/.local/share/applications/stv2.desktop` and the `stv2.png` icons under `~/.local/share/icons/hicolor/`
- `~/.local/share/syncthing-v2`, which holds settings and logs, except a kept Syncthing in its `syncthing/` folder
- with `--remove-syncthing`: the downloaded Syncthing and its start-at-login entry, either the `stv2-syncthing.service` user unit or `~/.config/autostart/stv2-syncthing.desktop`

A distribution Syncthing (`syncthing.service`) is never removed. If SyncThing V2 enabled that unit for you, it stays enabled after a normal uninstall. With `--remove-syncthing`, it is disabled again (`systemctl --user disable syncthing.service`), and SyncThing V2's marker drop-in for it is removed. Syncthing itself stays installed.

**`.deb`:** run `stv2 uninstall` first, which removes your per-user entries and settings, then remove the package:

```sh
sudo apt remove syncthing-v2
```

## Removing Syncthing's data as well

This is a separate step, and you rarely need it. SyncThing V2 never does it. If you really want Syncthing's configuration and database gone, stop Syncthing first, then delete its home directory:

| OS | Syncthing's home directory |
|---|---|
| Windows | `%LOCALAPPDATA%\Syncthing` |
| macOS | `~/Library/Application Support/Syncthing` |
| Linux | `~/.local/state/syncthing` (or `~/.config/syncthing` for older installs) |

This removes the device identity and folder list, not the synced files. Your folders stay wherever they are.
