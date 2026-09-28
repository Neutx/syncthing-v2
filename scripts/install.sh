#!/bin/sh
# SyncThing V2 installer for macOS and Linux. It never uses sudo and installs for the current user.
#
#   curl -fsSL https://github.com/Neutx/syncthing-v2/releases/latest/download/install.sh | sh
#
# Options (when piping, pass them as: ... | sh -s -- --uninstall):
#   --uninstall    run "stv2 uninstall --yes" for the installed copy
#   --allow-root   allow running as root (installs for root only)
# Environment:
#   STV2_VERSION   release to install (x.y.z); defaults to the version baked in at release time
#   STV2_BASE_URL  base URL of the release assets (used by CI)
set -eu

BAKED_VERSION='__STV2_VERSION__'
REPO='Neutx/syncthing-v2'
APP_NAME='SyncThing V2.app'

say() { printf '%s\n' "$*"; }
usage() {
    say "Usage: install.sh [--uninstall] [--allow-root]"
    say "  --uninstall    uninstall SyncThing V2 for the current user"
    say "  --allow-root   allow running as root (installs for root only)"
    say "Environment: STV2_VERSION (x.y.z), STV2_BASE_URL (release asset base URL)"
}
die() {
    printf 'install.sh: %s\n' "$*" >&2
    exit 1
}
# quit_running asks the tray of an installed copy to exit. It is bounded: the
# command is stopped after 15 seconds, so a copy that ignores --quit can never
# hang the installer. "stv2 install" also stops a running tray itself.
quit_running() {
    "$1" --quit </dev/null >/dev/null 2>&1 &
    qpid=$!
    waited=0
    while kill -0 "$qpid" 2>/dev/null; do
        if [ "$waited" -ge 15 ]; then
            kill "$qpid" 2>/dev/null || true
            break
        fi
        sleep 1
        waited=$((waited + 1))
    done
    wait "$qpid" 2>/dev/null || true
}

uninstall=0
allow_root=0
for arg in "$@"; do
    case $arg in
        --uninstall) uninstall=1 ;;
        --allow-root) allow_root=1 ;;
        -h | --help)
            usage
            exit 0
            ;;
        *) die "unknown option: $arg (supported: --uninstall, --allow-root)" ;;
    esac
done

# 1. Refuse root unless explicitly allowed: SyncThing V2 is a per-user app.
if [ "$(id -u)" -eq 0 ] && [ "$allow_root" -ne 1 ]; then
    die "do not run this as root; it installs for the current user. Use --allow-root to override."
fi
if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ]; then
    die "HOME is not set to an existing directory"
fi

# 2. Platform.
os=$(uname -s)
arch=$(uname -m)
case $os in
    Darwin) platform=macos-universal ;;
    Linux)
        case $arch in
            x86_64 | amd64) platform=linux-x64 ;;
            aarch64 | arm64) platform=linux-arm64 ;;
            *) die "unsupported Linux architecture: $arch (x86_64 and aarch64 are supported)" ;;
        esac
        ;;
    *) die "unsupported operating system: $os (macOS and Linux are supported)" ;;
esac

# 7. --uninstall: hand over to the installed copy.
if [ "$uninstall" -eq 1 ]; then
    if [ "$os" = Darwin ]; then
        set -- "$HOME/Applications/$APP_NAME/Contents/MacOS/stv2" "/Applications/$APP_NAME/Contents/MacOS/stv2"
    else
        set -- "$HOME/.local/share/syncthing-v2/bin/stv2" "$HOME/.local/bin/stv2" "$(command -v stv2 || true)"
    fi
    for bin in "$@"; do
        if [ -n "$bin" ] && [ -x "$bin" ]; then
            "$bin" uninstall --yes </dev/null
            say "SyncThing V2 was uninstalled. Syncthing's configuration and your synced folders were not touched."
            exit 0
        fi
    done
    die "SyncThing V2 is not installed for this user"
fi

# 3. Version and download location.
ver=${STV2_VERSION:-$BAKED_VERSION}
ver=${ver#v}
if ! printf '%s\n' "$ver" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$'; then
    die "this copy of install.sh has no release version; set STV2_VERSION=x.y.z and run it again"
fi
base=${STV2_BASE_URL:-https://github.com/$REPO/releases/download/v$ver}
base=${base%/}
asset="SyncThingV2-$ver-$platform.tar.gz"

if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
else
    die "curl or wget is required"
fi

# 4. Download into a private temp directory that is always removed.
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t stv2)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

say "Downloading SyncThing V2 $ver ($platform) from $base"
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/SHA256SUMS.txt" "$tmp/SHA256SUMS.txt" || die "download failed: $base/SHA256SUMS.txt"

# 5. Verify the SHA-256 against the matching SHA256SUMS.txt line.
awk -v f="$asset" '{ n = $2; sub(/^\*/, "", n); if (n == f) print $1 "  " n }' \
    "$tmp/SHA256SUMS.txt" >"$tmp/check.txt"
[ -s "$tmp/check.txt" ] || die "SHA256SUMS.txt has no entry for $asset; nothing was installed"
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$tmp" && sha256sum -c check.txt >/dev/null 2>&1) || die "checksum mismatch for $asset; nothing was installed"
elif command -v shasum >/dev/null 2>&1; then
    (cd "$tmp" && shasum -a 256 -c check.txt >/dev/null 2>&1) || die "checksum mismatch for $asset; nothing was installed"
else
    die "sha256sum or shasum is required to verify the download"
fi
say "Checksum verified."

mkdir "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x"

# 6. Install.
if [ "$os" = Darwin ]; then
    [ -x "$tmp/x/$APP_NAME/Contents/MacOS/stv2" ] || die "$asset does not contain $APP_NAME"
    dest="$HOME/Applications/$APP_NAME"
    mkdir -p "$HOME/Applications"
    if [ -d "$dest" ]; then
        say "Replacing the existing $dest"
        if [ -x "$dest/Contents/MacOS/stv2" ]; then
            quit_running "$dest/Contents/MacOS/stv2"
        fi
        rm -rf "$dest"
    fi
    mv "$tmp/x/$APP_NAME" "$dest"
    "$dest/Contents/MacOS/stv2" install --yes </dev/null
else
    [ -f "$tmp/x/stv2" ] || die "$asset does not contain stv2"
    chmod 0755 "$tmp/x/stv2"
    (cd "$tmp/x" && ./stv2 install --yes </dev/null)
    case ":${PATH:-}:" in
        *":$HOME/.local/bin:"*) ;;
        *) say "Note: add ~/.local/bin to your PATH to run stv2 from a terminal." ;;
    esac
fi

say ""
say "SyncThing V2 $ver is installed."
say "Open SyncThing V2 from the tray or menu bar. Do the same on your other computer, then click Pair."
