# Contributing to SyncThing V2

Thanks for helping. Bug reports, platform reports (especially from macOS and Linux, which are still **Preview**), documentation fixes and code are all welcome.

- **Bugs and platform reports:** open an [issue](https://github.com/Neutx/syncthing-v2/issues/new/choose) and fill in the template. Include the output of `stv2 version` and `stv2 doctor`, plus the redacted text from **Settings → Copy diagnostics**.
- **Security problems:** do not open an issue. Follow [SECURITY.md](SECURITY.md).
- **Larger changes:** open an issue first, so we can agree on the approach before you spend time on it.

## Setting up

1. Install Go at the version in `go.mod` (see [docs/building.md](docs/building.md)). On macOS, also install the Xcode Command Line Tools.
2. Clone the repository and enable the pre-commit hook:

   ```sh
   git config core.hooksPath .githooks
   ```

   The hook runs the privacy check on your staged changes, warns if your commit identity contains denylisted text, and runs `gitleaks` if it is installed.
3. Build and test:

   ```sh
   go build ./...
   go test ./...
   ```

   On Windows, `scripts\build.ps1` builds `dist\stv2.exe` the same way a release does.

To try the dashboard without Syncthing or Tailscale, run `stv2 demo` (see [docs/building.md](docs/building.md#demo-mode)). To try the tray against a throwaway Syncthing, follow [Running against a sandbox Syncthing](docs/building.md#running-against-a-sandbox-syncthing). **Never test against your real Syncthing configuration.**

## Tests

- Put unit tests next to the code, in `*_test.go`. CI runs `go test -race ./...` on Windows, macOS and Linux, so keep tests portable. Use build tags for OS-specific tests, and never depend on the machine you run on.
- **Fixtures are synthetic only.** Use:
  - generated device IDs
  - RFC 5737 addresses (`192.0.2.x`, `198.51.100.x`, `203.0.113.x`) or `100.64.0.x` for the tailnet
  - `example` host names (`laptop.example-tailnet.ts.net`)
  - `example.com` logins

  Never paste real `config.xml` files, `tailscale status` output, logs or device IDs, not even your own.
- Things that talk to Syncthing are tested against `httptest` fakes. Things that talk to Tailscale use a fake `tailnet.Source`. Nothing in the shipped binary reads environment variables for test purposes.
- The end-to-end pairing test (`go test -tags e2e ./e2e/...`) starts two real Syncthing instances in temporary directories. Run it when you change pairing, the Syncthing client or `stinstall`.

## Style

- `gofmt -l .` must print nothing. `go vet ./...` and `staticcheck ./...` must pass. CI also runs `govulncheck`, `go mod tidy -diff`, `shellcheck`, `actionlint` and PSScriptAnalyzer.
- Keep the dependency list short. The runtime modules are listed in [docs/architecture.md](docs/architecture.md). Prefer the standard library. A new module needs a good reason, and an entry in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
- User-visible names come from `internal/brand`. Do not hard-code "SyncThing V2", the binary name or identifiers elsewhere.
- Run external programs with argument lists, never through a shell. Never log secrets: the Syncthing API key must not reach the log, the dashboard page or diagnostics.
- Shell scripts are POSIX `sh` unless they say otherwise, and must pass `shellcheck`. PowerShell scripts must run on Windows PowerShell 5.1, and are saved as ASCII with CRLF line endings.
- Commit messages: a short summary line in the imperative ("Fix tooltip truncation on Windows"), a blank line, then the why.

## UI rule: no hover raise

**Never add a hover-to-raise effect** to the dashboard. Hover may change only an element's fill, rim (border) brightness or text colour. These are forbidden:

- `translateY` or other transforms
- scale
- `box-shadow` lift
- `top` or `margin-top` offsets
- `filter: drop-shadow`
- any other elevation on hover

`internal/ui/hover_lint_test.go` enforces this and fails the build. Press feedback (the spring "gel press" on pointer down) is allowed. See [docs/design.md](docs/design.md) for the design tokens and motion rules. Keep `prefers-reduced-motion` working when you add animation.

## Privacy

The repository is public. Nothing personal may enter it: no real host names, user names, email addresses, tailnet names, IP addresses, device or folder IDs, or personal paths. This applies to code, tests, docs, commit messages and screenshots.

- Screenshots come only from demo mode (`scripts\screenshots.ps1`). Look at every image before you commit it.
- Maintainers keep a private denylist in `.git/info/privacy-denylist` (untracked; one literal per line) and in the `PRIVACY_DENYLIST` CI secret. `scripts/privacy-check.sh` (or `.ps1`) checks the tree, the staged changes (`--staged`) or the full history (`--history`) against it, without printing the matched text.
- Your commit author name and email become public when your change is merged. Consider a GitHub noreply address (`git config user.email <id>+<user>@users.noreply.github.com`).

## Pull request checklist

- [ ] `gofmt -l .` is empty, and `go vet ./...` and `go test ./...` pass locally.
- [ ] Tests cover the change, with synthetic fixtures only.
- [ ] There is no personal data in the diff, the commit messages or the screenshots, and the privacy hook is enabled.
- [ ] UI changes have no hover-to-raise effect.
- [ ] Nothing is accepted without a click on that machine, no new network listener is added, and Syncthing's GUI and API stay on loopback.
- [ ] Changed shell or PowerShell scripts pass shellcheck or PSScriptAnalyzer.
- [ ] Docs are updated when behaviour changes, and `CHANGELOG.md` has an entry under `## [Unreleased]`.

By contributing, you agree that your contribution is licensed under the project's [MIT licence](LICENSE).
