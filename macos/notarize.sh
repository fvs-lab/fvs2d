#!/bin/bash
set -euo pipefail
APP=${1:?Usage: notarize.sh /path/to/FVS.app}
: "${NOTARY_PROFILE:?Set NOTARY_PROFILE to a notarytool keychain profile}"
APP=$(cd "$(dirname "$APP")" && pwd)/$(basename "$APP")
ARCHIVE="${APP%.app}-notarization.zip"
codesign --verify --deep --strict "$APP"
ditto -c -k --keepParent "$APP" "$ARCHIVE"
xcrun notarytool submit "$ARCHIVE" --keychain-profile "$NOTARY_PROFILE" --wait
xcrun stapler staple "$APP"
xcrun stapler validate "$APP"
spctl --assess --type execute --verbose "$APP"
