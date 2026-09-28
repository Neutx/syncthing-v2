#!/bin/sh
# Build the drag-to-Applications disk image for SyncThing V2.
#
# Usage: packaging/macos/make-dmg.sh <app-bundle> <out.dmg>
#   <app-bundle>  the "SyncThing V2.app" made by make-app.sh
#   <out.dmg>     output path, e.g. SyncThingV2-<v>-macos-universal.dmg (replaced if it exists)
#
# The image holds a copy of the bundle next to a symlink to /Applications and is
# compressed with UDZO.
set -eu

die() {
    printf 'make-dmg.sh: %s\n' "$*" >&2
    exit 1
}

[ "$#" -eq 2 ] || die "usage: make-dmg.sh <app-bundle> <out.dmg>"
app=${1%/}
dmg=$2

if [ ! -f "$app/Contents/Info.plist" ] || [ ! -d "$app/Contents/MacOS" ]; then
    die "not an app bundle: $app"
fi
for tool in hdiutil ditto; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool not found; run this on macOS"
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

stage="$tmp/stage"
mkdir "$stage"
# ditto keeps the code signature, extended attributes and symlinks intact.
ditto "$app" "$stage/$(basename "$app")"
ln -s /Applications "$stage/Applications"

mkdir -p "$(dirname "$dmg")"
rm -f "$dmg"
hdiutil create -volname "SyncThing V2" -srcfolder "$stage" -fs HFS+ \
    -format UDZO -imagekey zlib-level=9 -ov "$dmg"
hdiutil verify "$dmg"

printf 'Built %s\n' "$dmg"
