#!/bin/sh
# Assemble "SyncThing V2.app" (macOS, universal) from cgo-built stv2 binaries.
#
# Usage: packaging/macos/make-app.sh <version> <out-dir> <binary> [<binary>...]
#   <version>  release version, x.y.z with an optional -suffix; the bundle gets the numeric x.y.z
#   <out-dir>  directory that receives "SyncThing V2.app" (an existing bundle there is replaced)
#   <binary>   darwin stv2 builds, typically one arm64 and one amd64. Two or more are merged
#              with `lipo -create`; a single binary is used as is.
# Environment:
#   STV2_CODESIGN_IDENTITY  codesign identity. The default "-" signs ad hoc with --timestamp=none;
#                           any other identity signs with the hardened runtime and a timestamp.
#
# Steps: lipo, bundle layout, Info.plist (version filled in), stv2.icns built with iconutil from
# assets/icons, then codesign of the binary followed by codesign of the bundle.
# The release tarball is made afterwards with:
#   tar -C <out-dir> -czf SyncThingV2-<v>-macos-universal.tar.gz "SyncThing V2.app"
set -eu

die() {
    printf 'make-app.sh: %s\n' "$*" >&2
    exit 1
}

[ "$#" -ge 3 ] || die "usage: make-app.sh <version> <out-dir> <binary> [<binary>...]"
version=${1#v}
out_dir=$2
shift 2

numeric=${version%%[-+]*}
printf '%s\n' "$numeric" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
    die "'$version' is not a semantic version (x.y.z[-suffix])"

for tool in lipo iconutil codesign plutil xattr; do
    command -v "$tool" >/dev/null 2>&1 ||
        die "$tool not found; run this on macOS with the Xcode command line tools installed"
done
for bin in "$@"; do
    [ -f "$bin" ] || die "binary not found: $bin"
done

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
icons=$(CDPATH='' cd -- "$here/../../assets/icons" 2>/dev/null && pwd) ||
    die "assets/icons not found; run: go run ./internal/icon/cmd/genicons -out assets/icons"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

mkdir -p "$out_dir"
app="$out_dir/SyncThing V2.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

# 1. Executable: merge the per-arch builds into one universal binary.
exe="$app/Contents/MacOS/stv2"
if [ "$#" -eq 1 ]; then
    cp "$1" "$exe"
else
    lipo -create -output "$exe" "$@"
fi
chmod 0755 "$exe"
printf 'Architectures: %s\n' "$(lipo -archs "$exe")"

# 2. Info.plist and PkgInfo.
sed "s/__STV2_VERSION__/$numeric/g" "$here/Info.plist" >"$app/Contents/Info.plist"
plutil -lint "$app/Contents/Info.plist" >/dev/null || die "Info.plist is not valid"
printf 'APPL????' >"$app/Contents/PkgInfo"

# 3. Icon: an .iconset with every size iconutil expects, from the PNGs genicons renders
#    at each exact size (assets/icons/icon-<N>.png).
iconset="$tmp/stv2.iconset"
mkdir "$iconset"
for entry in 16x16:16 16x16@2x:32 32x32:32 32x32@2x:64 128x128:128 128x128@2x:256 \
    256x256:256 256x256@2x:512 512x512:512 512x512@2x:1024; do
    src="$icons/icon-${entry##*:}.png"
    [ -f "$src" ] || die "$src not found; run: go run ./internal/icon/cmd/genicons -out assets/icons"
    cp "$src" "$iconset/icon_${entry%%:*}.png"
done
iconutil -c icns -o "$app/Contents/Resources/stv2.icns" "$iconset"

# 4. Signing: the binary first, then the bundle (which seals Info.plist and the resources).
xattr -cr "$app"
identity=${STV2_CODESIGN_IDENTITY:--}
if [ "$identity" = "-" ]; then
    set -- --force --sign - --timestamp=none
else
    set -- --force --sign "$identity" --timestamp --options runtime
fi
codesign "$@" "$exe"
codesign "$@" "$app"
codesign --verify --strict --verbose=2 "$app"

printf 'Built %s (%s)\n' "$app" "$numeric"
