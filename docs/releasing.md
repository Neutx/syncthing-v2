# Releasing

Releases are built by `.github/workflows/release.yml` when a version tag is pushed. The workflow creates a **draft** GitHub release. A maintainer publishes it by hand after the checklist below. The one-line installers resolve `releases/latest`, so nobody receives a release until it is published.

## 1. Prepare

1. Make sure `main` is green in CI on all three operating systems, including `installer-test`.
2. Update `CHANGELOG.md`:
   - Move the entries under `## [Unreleased]` into a new `## [x.y.z] - YYYY-MM-DD` section, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style.
   - For the first release, change `## [1.0.0] - Unreleased` to the real date.
   - Update the link definitions at the end of the file.

   The release workflow refuses a tag that has no `## [x.y.z]` section, and that section's text becomes the release notes.
3. If the bootstrap Syncthing should move to a newer upstream release, [bump the pin](#bumping-the-syncthing-pin) first.
4. Build and test locally on Windows (`scripts\build.ps1`, and `go test -tags e2e ./e2e/...`).
5. Refresh the screenshots if the dashboard changed (`scripts\screenshots.ps1`, see [building.md](building.md#screenshots)), and look at every image.

## 2. Tag

```sh
git tag -a v1.0.0 -m "SyncThing V2 1.0.0"
git push origin v1.0.0
```

Tags must be semantic versions: `vX.Y.Z`, or `vX.Y.Z-suffix` for a pre-release, which is then marked as a pre-release.

## 3. What the workflow does

1. **ci:** re-runs lint, lint-ps, test and e2e from `ci.yml` on the tagged commit.
2. **verify:**
   - checks the tag format and the `CHANGELOG.md` section
   - records the commit time as `SOURCE_DATE_EPOCH`
   - bakes the version into `install.ps1` and `install.sh` by replacing the `__STV2_VERSION__` marker
3. **build-windows:** go-winres resources, then `SyncThingV2-Setup-<v>-windows-x64.exe` (`-trimpath -buildvcs=true -H windowsgui`).
4. **build-linux:** static binaries for amd64 and arm64, packed as `SyncThingV2-<v>-linux-{x64,arm64}.tar.gz` (reproducible tar settings), and `syncthing-v2_<v>_{amd64,arm64}.deb` built with nfpm.
5. **build-macos:** two cgo builds (arm64, and amd64 with `CC="clang -arch x86_64"`), merged with `lipo` into `SyncThing V2.app`, signed ad hoc, then `SyncThingV2-<v>-macos-universal.dmg` and `.tar.gz`.
6. **installer-test:** serves the exact release assets and the baked scripts on `127.0.0.1:8000`, runs the documented one-liners against them on all three OSes, then checks the following:
   - `stv2 version`
   - `stv2 doctor --json`
   - the autostart entry
   - `stv2 uninstall --yes`
7. **publish:**
   - writes `SHA256SUMS.txt` over every asset, including both scripts
   - creates a build provenance attestation for every asset
   - runs `gh release create v<v> --draft --title "SyncThing V2 <v>"` with the CHANGELOG section as notes
   - uploads everything

**Optional signing.** These steps are present in the workflow but skipped unless their secrets exist:

- **Authenticode through the SignPath Foundation:** the `SIGNPATH_API_TOKEN` secret, plus the variables `SIGNPATH_ORGANIZATION_ID`, `SIGNPATH_PROJECT_SLUG` and `SIGNPATH_SIGNING_POLICY_SLUG`.
- **Apple Developer ID signing and notarization:** `APPLE_DEVELOPER_ID_P12` (base64), `APPLE_ID`, `APPLE_TEAM_ID` and `APPLE_APP_PASSWORD`, plus `APPLE_DEVELOPER_ID_P12_PASSWORD` if the `.p12` has a password.

Release 1.0 ships unsigned.

**Privacy check.** The CI privacy check needs the `PRIVACY_DENYLIST` repository secret: one literal identifier per line. Without it, the check is skipped with a notice, as it is on forks.

## 4. Human release checklist

Do these steps on the draft, in order:

1. **VirusTotal.** Upload `SyncThingV2-Setup-<v>-windows-x64.exe` to [virustotal.com](https://www.virustotal.com/). If any engine flags it, submit the file to the [Microsoft Defender false-positive portal](https://www.microsoft.com/wdsi/filesubmission) (and to the other vendors that flag it). Wait for clearance before publishing.
2. **Smoke install from the draft assets** on a clean Windows VM or Windows Sandbox. Also do it on a macOS and a Linux machine if available. Download the assets from the draft, check them against `SHA256SUMS.txt`, install them, and run `stv2 version` and `stv2 doctor`.
3. **Live two-computer test over Tailscale:**
   1. Install on both computers with the one-liner. Before publishing, set `STV2_BASE_URL` to a local copy of the draft assets.
   2. Click **Pair** on one computer and **Accept** on the other.
   3. Share a folder and accept it.
   4. Check that a file syncs in both directions.
   5. Reboot one computer and check that sync resumes by itself.
4. **Publish** the draft. The one-liners resolve `releases/latest` only once the release is published.

After publishing, check that the one-liners from the README install the new version on a machine, and close the milestone.

## Bumping the Syncthing pin

SyncThing V2 downloads a pinned Syncthing release when none is installed. The pin is `brand.BootstrapSyncthing` plus the SHA-256 table in `internal/stinstall/pins.go`. Both are generated:

```sh
scripts/update-syncthing-pin.sh v2.1.5
```

The script needs `bash`, `curl`, `gpg`, `awk`, `sed`, `grep` and `mktemp`. It does the following:

1. downloads the release's `sha256sum.txt.asc`
2. verifies its signature against `build/syncthing-release-key.asc` (Syncthing's release key, whose fingerprint is written in the script)
3. writes the checksums of the Windows, macOS and Linux assets that SyncThing V2 downloads into `pins.go`, and sets `BootstrapSyncthing` to the same version

It refuses to write anything if the signature does not verify. Commit the two changed files together, along with these updates:

- the Syncthing version and source link in `THIRD_PARTY_NOTICES.md`
- a note of the new version in `CHANGELOG.md`

Then run the e2e test, which downloads the newly pinned release.

If Syncthing ever rotates its release key, replace `build/syncthing-release-key.asc` only after checking the new key through at least two independent channels. The script's header lists the checks that were done for the current key. `brand.MinSyncthing` (the oldest supported Syncthing, doctor code ST004) is changed by hand, and only for a real API need.
