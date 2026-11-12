#!/bin/bash
set -euo pipefail

if [[ $(uname -s) != Darwin ]]; then
    echo "The FSKit extension requires macOS and Xcode with the macOS 26 SDK." >&2
    exit 1
fi
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-"$HOME/artifacts/fvs-macos-build"}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
mkdir -p "$OUT/tmp"
export TMPDIR="$OUT/tmp" TMP="$OUT/tmp" TEMP="$OUT/tmp"
SDK=$(xcrun --sdk macosx --show-sdk-path)
SDK_VERSION=$(xcrun --sdk macosx --show-sdk-version)
if [[ ${SDK_VERSION%%.*} -lt 26 ]]; then
    echo "macOS 26 SDK required, found $SDK_VERSION" >&2
    exit 1
fi
ARCH=${ARCH:-$(uname -m)}
case "$ARCH" in
    arm64) GOARCH=arm64 ;;
    x86_64) GOARCH=amd64 ;;
    *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
export GOARCH GOOS=darwin
APP="$OUT/FVS.app"
EXT="$APP/Contents/Extensions/FVS.appex"
if [[ -e "$APP" ]]; then
    echo "Use a fresh OUT directory; $APP already exists." >&2
    exit 1
fi
mkdir -p "$APP/Contents/MacOS" "$EXT/Contents/MacOS"
cp "$ROOT/macos/App/Info.plist" "$APP/Contents/Info.plist"
cp "$ROOT/macos/Extension/Info.plist" "$EXT/Contents/Info.plist"
cd "$ROOT"
CGO_ENABLED=0 go build -trimpath -o "$APP/Contents/MacOS/fvs2d" ./cmd/fvs2d
CGO_ENABLED=1 CC="$(xcrun -f clang)" \
    CGO_CFLAGS="-isysroot $SDK -mmacosx-version-min=26.0 -arch $ARCH" \
    CGO_LDFLAGS="-isysroot $SDK -mmacosx-version-min=26.0 -arch $ARCH" \
    go build -trimpath -buildmode=c-archive -o "$OUT/libfvs.a" ./cmd/fvsbridge
xcrun swiftc -O -swift-version 5 -sdk "$SDK" -target "$ARCH-apple-macos26.0" \
    -application-extension -parse-as-library -import-objc-header "$OUT/libfvs.h" \
    "$ROOT"/macos/Extension/*.swift "$OUT/libfvs.a" \
    -framework FSKit -framework Foundation -framework CoreFoundation -framework Security \
    -lresolv -o "$EXT/Contents/MacOS/FVS"
xcrun swiftc -O -swift-version 5 -sdk "$SDK" -target "$ARCH-apple-macos26.0" \
    "$ROOT/macos/App/main.swift" -framework AppKit -o "$APP/Contents/MacOS/FVS"

xcrun swiftc -O -swift-version 5 -sdk "$SDK" -target "$ARCH-apple-macos26.0" \
    -parse-as-library "$ROOT/macos/Control/main.swift" -framework FSKit -framework Foundation \
    -o "$APP/Contents/MacOS/fvsfsctl"

if [[ -n ${SIGN_IDENTITY:-} ]]; then
    if [[ -n ${EXTENSION_PROFILE:-} ]]; then
        cp "$EXTENSION_PROFILE" "$EXT/Contents/embedded.provisionprofile"
    fi
    codesign --force --options runtime --sign "$SIGN_IDENTITY" "$APP/Contents/MacOS/fvs2d"
    codesign --force --options runtime --sign "$SIGN_IDENTITY" "$APP/Contents/MacOS/fvsfsctl"
    codesign --force --options runtime --entitlements "$ROOT/macos/Extension/Extension.entitlements" \
        --sign "$SIGN_IDENTITY" "$EXT"
    codesign --force --options runtime --sign "$SIGN_IDENTITY" "$APP"
    codesign --verify --deep --strict --verbose=2 "$APP"
else
    echo "Unsigned build. Installation and real mount tests need valid FSKit signing credentials."
fi
printf '%s\n' "$APP"
