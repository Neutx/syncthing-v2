# Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

> **Status:** signing through the SignPath Foundation is not active yet. Until a release says otherwise, releases are **not** Authenticode-signed. This page is the policy that applies to every signed release.

## What is signed

- **Signed:** only the Windows installer, `SyncThingV2-Setup-<version>-windows-x64.exe`.
- **How it is built:** `.github/workflows/release.yml` builds it from a version tag of this repository, on GitHub-hosted runners, with no build cache. The workflow uploads the unsigned file as a workflow artifact, and the SignPath GitHub Actions integration submits it for signing. SignPath checks that the request comes from that workflow run. The installer is never signed from a developer's computer.
- **Approval:** a person from the approvers list below approves every signing request by hand in SignPath.
- **File metadata:** the product name is `SyncThing V2`, and the product and file versions are the release version.
- **Not signed with this certificate:**
  - Syncthing. SyncThing V2 does not bundle it. If Syncthing is missing, SyncThing V2 downloads the official upstream release at run time and checks it against SHA-256 pins compiled into the program (see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)).
  - The Linux and macOS packages and the install scripts. Every release asset, signed or not, is listed in `SHA256SUMS.txt` and has a GitHub build provenance attestation.

## Team roles

| Role | Members |
|---|---|
| Committers and reviewers | [Neutx](https://github.com/Neutx) |
| Approvers | [Neutx](https://github.com/Neutx) |

- **Committers** may change the source code and the build.
- **Reviewers:** a committer reviews every change from someone who is not a committer before it is merged.
- **Approvers** approve each signing request.
- **Accounts:** all team members use multi-factor authentication on GitHub and on SignPath.

## Privacy policy

Apart from the connections listed below, this program will not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it.

The connections SyncThing V2 makes, all described in [docs/security.md](docs/security.md):

- **Update check.** Once a day, SyncThing V2 makes one unauthenticated request to `api.github.com` for the latest release.
  - It sends no cookies, tokens or identifiers. The only extra information is the `stv2/<version>` user agent.
  - It is on by default. Turn it off in **Settings → Check for updates**.
  - GitHub sees your IP address, as with any web request. See [GitHub's privacy statement](https://docs.github.com/en/site-policy/privacy-policies/github-general-privacy-statement).
- **Syncthing download.** SyncThing V2 downloads Syncthing from GitHub Releases only when Syncthing is not installed.
- **Device probes.** SyncThing V2 makes TLS probes to port 22000 of the devices on your own tailnet. It does this once at startup, and again when you open the Pair view or click Refresh.
- **Syncthing.** Syncthing, which SyncThing V2 installs and configures, syncs the folders you share with the devices you pair. Its own discovery, relaying and anonymous usage reporting are controlled by Syncthing's settings, and Syncthing asks you before it reports usage. See [Syncthing's security and privacy documentation](https://docs.syncthing.net/users/security.html).
- **Tailscale.** SyncThing V2 reads the local `tailscale status` and uses your tailnet. Tailscale's own data handling is covered by the [Tailscale privacy policy](https://tailscale.com/privacy-policy).

SyncThing V2 collects no telemetry. Diagnostics and logs stay on your computer unless you copy and send them yourself.
