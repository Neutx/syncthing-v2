# Building from source

## Toolchain

- **Go**, at the version in `go.mod` (`go 1.27`, `toolchain go1.27.1`). With an older Go 1.21+ installed, `go` downloads the right toolchain by itself. Get Go from [go.dev/dl](https://go.dev/dl/).
  - **Windows:** the `.zip` works without admin rights. Extract it, for example to `%LOCALAPPDATA%\Programs\Go`, and use `bin\go.exe`. `scripts\build.ps1` also looks there when `go` is not on `PATH`.
- **Windows and Linux** build with `CGO_ENABLED=0`, so no C compiler is needed.
- **macOS** needs cgo for the Cocoa tray. Install the Xcode Command Line Tools with `xcode-select --install`.
- Build-time tools run with `go run <module>@<pinned version>`, so there is nothing else to install:
  - `github.com/tc-hib/go-winres` for the Windows icon, manifest and version resources
  - `github.com/goreleaser/nfpm/v2/cmd/nfpm` for the `.deb`

No Node.js, npm or bundler is involved. The dashboard is plain HTML, CSS and JavaScript in `internal/ui/web/`, embedded with `go:embed`.

## Windows: `scripts\build.ps1`

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\build.ps1 -Version 1.0.0
```

The script:

1. generates the exe resources (icon, manifest, version information) with go-winres into `cmd\stv2\rsrc_windows_amd64.syso`, which is not committed
2. builds `dist\stv2.exe` for windows/amd64 with `-trimpath`, `-H windowsgui` and the version linked in
3. runs `go test ./...` (skip this with `-SkipTests`)

`-Version` defaults to `0.0.0-dev`. The resulting exe is the same self-installing program as a release. Running it offers to install it for your user.

## macOS and Linux: `scripts/build.sh`

```sh
sh scripts/build.sh 1.0.0
```

It builds `dist/stv2` for the host, runs the tests, and honours these environment variables:

- `GOOS` / `GOARCH` to cross-compile
- `CGO_ENABLED` (defaults to 1 on macOS and 0 elsewhere)
- `SKIP_TESTS=1`
- `OUT` for another output path
- `GO` for another `go` command

With `GOOS=windows` it also runs go-winres, so CI can build the Windows exe from Linux.

Packaging, as the release workflow does it:

- **macOS app and disk image:** `sh packaging/macos/make-app.sh <version> <outdir> <stv2-arm64> [<stv2-amd64>]` makes `SyncThing V2.app`. It merges the binaries with `lipo`, builds the `.icns` and signs ad hoc. Then `sh packaging/macos/make-dmg.sh "<outdir>/SyncThing V2.app" <file>.dmg`.
- **Debian package:** `STV2_VERSION=<version> STV2_ARCH=amd64 STV2_BINARY=<path to stv2> go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0 package --config packaging/linux/nfpm.yaml -p deb --target dist/`

## Tests

```sh
go test ./...                 # unit tests
go test -race ./...           # what CI runs on Windows, macOS and Linux (needs cgo)
go vet ./...
gofmt -l .                    # must print nothing
```

Unit tests use synthetic fixtures only (`testdata/` folders): made-up device IDs, `100.64.0.x` and RFC 5737 addresses, and `example` host names. They include:

- the device-ID vectors
- the pairing policy truth table
- the probe against an in-process TLS server, which asserts that no client certificate is ever sent
- the status engine against a fake Syncthing REST server
- golden images for the icons and the glass pipeline
- the dashboard auth matrix
- the **hover lint** (`internal/ui/hover_lint_test.go`), which enforces the no-hover-raise rule

The CI lint job also runs:

- `staticcheck` and `govulncheck` (pinned versions are in `.github/workflows/ci.yml`)
- `go mod tidy -diff`
- `shellcheck` on the shell scripts
- `actionlint`
- `gitleaks`
- the privacy check

The `lint-ps` job runs PSScriptAnalyzer on `scripts/*.ps1`.

### End-to-end pairing test

```sh
go test -tags e2e -count=1 -timeout 15m ./e2e/...
```

This downloads the pinned Syncthing release (checksum-verified) and starts two real instances in temporary home directories. Their GUIs are on `127.0.0.1:18384` and `127.0.0.1:28384`, and they sync on `127.0.0.2:22000` and `127.0.0.3:22000`, with discovery, relays and upgrades off. The test then runs the whole flow: probe, pair, verify, accept, share a folder, sync a file, widen the addresses, simultaneous pairing and the identity-mismatch case. It never touches your own Syncthing. It runs on Linux and Windows. macOS skips it, because it configures only `127.0.0.1` on the loopback interface.

## Demo mode

```sh
stv2 demo --port 18999
```

`stv2 demo` serves the dashboard on `127.0.0.1` with synthetic data. It needs no Syncthing and no Tailscale, and it has made-up devices, folders and activity. The glass backdrop is rendered from a synthetic wallpaper. The command prints a single-use login link (valid for 30 s). Open it in a browser. For more links, POST to `/api/launch` with the control token the command prints and the header `X-STV2: 1`:

```sh
curl -s -X POST -H "Authorization: Bearer <token>" -H "X-STV2: 1" -d '{"next":"/?mode=glass&demo=syncing"}' http://127.0.0.1:18999/api/launch
```

The `demo` query parameter selects a scenario:

- `status`, `syncing`, `scanning`, `disconnected`, `paused`
- `error`, `down`, `unauthorized`
- `notices` (every banner)
- `pair`, `settings`

The `mode` parameter selects the material: `glass` (Windows), `vibrancy` or `browser`. Actions work against the synthetic state, so you can click through the UI safely.

## Screenshots

The README images in `assets/screenshots/` are real captures of demo mode, taken with Microsoft Edge in headless mode:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\screenshots.ps1
```

The script:

1. builds `stv2`
2. starts `stv2 demo --port 18999`
3. starts Edge headless with a throwaway profile and a 460×640 window
4. captures the `status`, `syncing`, `pair` and `settings` views through the DevTools protocol, at 2× scale, with reduced motion so every image shows the settled layout
   - views taller than the window (`pair`, `settings`) are scrolled just enough that neither image edge cuts a row or a section label in half
   - the `settings` view is captured a second time, scrolled to its end, as `settings-about.png`, which shows the About section with the version given by `-Version` (default `1.0.0`); the script fails if that version is not visible
5. renders the tray icon strip with `go run ./internal/icon/cmd/genicons -strip assets/screenshots/tray-states.png`

Options:

- `-Views` to pick scenarios
- `-Port`
- `-Scale`
- `-Version` for the version shown in `settings-about.png`
- `-Exe` to reuse a console build
- `-Edge` to point at `msedge.exe`

Look at every image before committing it. Screenshots must only ever show demo data: no real names, addresses, files or desktop.

## Running against a sandbox Syncthing

To try a development build without touching your real Syncthing, give it its own home directory and ports:

```sh
syncthing generate --home ./sandbox-st
syncthing serve --home ./sandbox-st --gui-address=127.0.0.1:18384 --no-browser --no-restart
```

Set the sync listener to `tcp://127.0.0.1:22001` in the sandbox web UI, or in `sandbox-st/config.xml` before the first start. Then start the tray with `STHOMEDIR` pointing at the sandbox, so config discovery picks it up:

```sh
STHOMEDIR=./sandbox-st ./dist/stv2
```

On Windows PowerShell, use `$env:STHOMEDIR = '.\sandbox-st'; .\dist\stv2.exe`.

Only one SyncThing V2 tray runs per user. Exit an installed copy first, or the development build just shows the dashboard of the running one.

## Regenerating generated files

- **App icons:** `go run ./internal/icon/cmd/genicons -out assets/icons` (the output is deterministic).
- **Syncthing pins:** `scripts/update-syncthing-pin.sh vX.Y.Z` (see [releasing.md](releasing.md#bumping-the-syncthing-pin)).
