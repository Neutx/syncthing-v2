#!/bin/sh
# Build SyncThing V2 for the target OS/arch into dist/ and run the tests.
#
# Usage: scripts/build.sh [version]
#   version       semantic version baked into the binary (default: $VERSION or 0.0.0-dev)
# Environment:
#   GOOS, GOARCH  cross-compile target (default: the host)
#   CGO_ENABLED   override the default (1 on macOS, which Cocoa needs; 0 elsewhere)
#   GO            Go command to use (default: go)
#   SKIP_TESTS=1  skip go test ./...
#   OUT           output path (default: dist/stv2, or dist/stv2.exe for Windows)
set -eu

MODULE="github.com/Neutx/syncthing-v2"
GO_WINRES="github.com/tc-hib/go-winres@v0.3.3"

VERSION="${1:-${VERSION:-0.0.0-dev}}"
if ! printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$'; then
    echo "build.sh: '$VERSION' is not a semantic version (x.y.z[-suffix])" >&2
    exit 2
fi
NUMERIC_VERSION="${VERSION%%[-+]*}"

GO="${GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "build.sh: Go was not found; install it from https://go.dev/dl/" >&2
    exit 1
fi

cd "$(dirname "$0")/.."

goos="$("$GO" env GOOS)"
goarch="$("$GO" env GOARCH)"
hostos="$("$GO" env GOHOSTOS)"
hostarch="$("$GO" env GOHOSTARCH)"

ldextra=""
case "$goos" in
    windows)
        default_out="dist/stv2.exe"
        default_cgo=0
        ldextra="-H windowsgui"
        if [ -f packaging/windows/winres.json ]; then
            echo "Generating exe resources ($GO_WINRES)"
            # go-winres runs on this machine, so build it for the host; --arch picks the target.
            GOOS="$hostos" GOARCH="$hostarch" CGO_ENABLED=0 "$GO" run "$GO_WINRES" make \
                --in packaging/windows/winres.json \
                --arch "$goarch" \
                --out cmd/stv2/rsrc \
                --product-version "$NUMERIC_VERSION" \
                --file-version "$NUMERIC_VERSION"
        else
            echo "packaging/windows/winres.json not found; building without exe resources."
        fi
        ;;
    darwin)
        default_out="dist/stv2"
        default_cgo=1
        ;;
    *)
        default_out="dist/stv2"
        default_cgo=0
        ;;
esac

OUT="${OUT:-$default_out}"
CGO_ENABLED="${CGO_ENABLED:-$default_cgo}"
export CGO_ENABLED

mkdir -p "$(dirname "$OUT")"
echo "Building $OUT ($VERSION, $goos/$goarch, CGO_ENABLED=$CGO_ENABLED)"
"$GO" build -trimpath \
    -ldflags "-s -w $ldextra -X $MODULE/internal/brand.Version=$VERSION" \
    -o "$OUT" ./cmd/stv2

if [ "${SKIP_TESTS:-0}" = "1" ]; then
    echo "Skipping tests (SKIP_TESTS=1)"
elif [ "$goos" != "$hostos" ] || [ "$goarch" != "$hostarch" ]; then
    echo "Cross-compiled for $goos/$goarch; skipping tests on $hostos/$hostarch"
else
    echo "Running go test ./..."
    "$GO" test ./...
fi

if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$OUT"
elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$OUT"
fi
echo "Built $OUT"
