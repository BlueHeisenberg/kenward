#!/bin/sh
# Build kenward.app and a .dmg around it.
#
#   packaging/macos/bundle.sh VERSION DESKTOP_BINARY KENWARD_BINARY [OUTDIR]
#
# Both binaries go inside the bundle. kenward-desktop looks for the daemon beside its
# own executable first, so a user who drags kenward.app to /Applications gets a working
# pair with nothing on PATH and no second installer.
#
# This must run on macOS: iconutil and hdiutil are macOS-only, and the desktop binary
# itself needs cgo for the menu bar, which means it was built on a Mac anyway.
#
# Nothing here signs or notarises anything. That is a deliberate scope decision and it
# has a consequence the user must be told about rather than discover: the first launch
# needs Finder → right-click → Open, because Gatekeeper refuses a double-clicked
# unsigned app with a message that reads like the download is damaged. docs/DESKTOP.md
# says so; so does the .dmg's own layout, which puts a README beside the app.
set -eu

VERSION="${1:?usage: bundle.sh VERSION DESKTOP_BINARY KENWARD_BINARY [OUTDIR]}"
DESKTOP_BIN="${2:?path to the kenward-desktop binary}"
KENWARD_BIN="${3:?path to the kenward binary}"
OUTDIR="${4:-dist}"

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)

APP="$OUTDIR/kenward.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

cp "$DESKTOP_BIN" "$APP/Contents/MacOS/kenward-desktop"
cp "$KENWARD_BIN" "$APP/Contents/MacOS/kenward"
chmod +x "$APP/Contents/MacOS/kenward-desktop" "$APP/Contents/MacOS/kenward"

sed "s/@VERSION@/$VERSION/g" "$HERE/Info.plist" > "$APP/Contents/Info.plist"

# The .icns, built here rather than committed: sips and iconutil ship with macOS, so
# the one source of truth stays packaging/kenward.png.
ICONSET=$(mktemp -d)/kenward.iconset
mkdir -p "$ICONSET"
for size in 16 32 64 128 256 512; do
	sips -z $size $size "$ROOT/packaging/kenward.png" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
	double=$((size * 2))
	sips -z $double $double "$ROOT/packaging/kenward.png" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/kenward.icns"

# A staging directory rather than `hdiutil create -srcfolder` over dist, so the .dmg
# holds exactly the app, the note about opening it, and the Applications symlink that
# makes the drag obvious.
STAGE=$(mktemp -d)/kenward
mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
cp "$HERE/FIRST-LAUNCH.txt" "$STAGE/"

DMG="$OUTDIR/kenward_${VERSION}_macos.dmg"
rm -f "$DMG"
hdiutil create -volname "kenward $VERSION" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null
echo "$DMG"
