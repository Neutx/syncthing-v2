## What and why

<!-- What does this change do, and which issue does it address (Fixes #123)? -->

## How it was tested

<!-- Commands you ran and platforms you checked (Windows, macOS, Linux). -->

## Checklist

- [ ] `gofmt -l .` is empty, and `go vet ./...` and `go test ./...` pass locally.
- [ ] Tests cover the change. Test fixtures are synthetic only: RFC 5737 or 100.64.0.x addresses, generated device IDs, `example` host names.
- [ ] No personal data anywhere in the diff, the commit messages or the screenshots: no real host names, user names, email addresses, tailnet names or IPs, device or folder IDs, or personal paths. The privacy hook is enabled (`git config core.hooksPath .githooks`).
- [ ] UI changes have no hover-to-raise effect: no `translateY`, `scale` or `box-shadow` lift on hover. Hover may only change fill or rim brightness.
- [ ] Nothing is accepted without a click on that machine, no new network listener is added, and Syncthing's GUI/API stay on loopback.
- [ ] Changed shell or PowerShell scripts pass shellcheck or PSScriptAnalyzer.
- [ ] Docs and `CHANGELOG.md` are updated when behaviour changes.
