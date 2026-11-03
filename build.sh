#!/bin/sh
# Builds the Windows and Mac downloads plus the files the in-app updater uses.
# Output goes to dist/. The version comes from the VERSION file (or $VERSION).
set -e
cd "$(dirname "$0")"
VERSION="${VERSION:-$(cat VERSION)}"
rm -rf dist && mkdir -p dist/win dist/mac
export CGO_ENABLED=0
LD="-s -w -X main.appVersion=$VERSION"

GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LD -H windowsgui" -o "dist/win/Media Converter.exe" .
cp "packaging/Read me first (Windows).txt" dist/win/

GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LD" -o dist/mc-arm64 .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$LD" -o dist/mc-amd64 .
APP="dist/mac/Media Converter.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
python3 packaging/lipo.py "$APP/Contents/MacOS/MediaConverter" dist/mc-amd64 dist/mc-arm64
chmod 755 "$APP/Contents/MacOS/MediaConverter"
sed "s/__VERSION__/$VERSION/g" packaging/Info.plist > "$APP/Contents/Info.plist"
printf 'APPL????' > "$APP/Contents/PkgInfo"
cp packaging/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
cp "packaging/Read me first (Mac).txt" dist/mac/
rm dist/mc-arm64 dist/mc-amd64

# Downloads for people installing for the first time
(cd dist/win && zip -q -X -r ../MediaConverter-Windows.zip .)
(cd dist/mac && zip -q -X -r -y ../MediaConverter-Mac.zip .)
# What the app downloads when it updates itself
cp "dist/win/Media Converter.exe" dist/MediaConverter-windows-amd64.exe
cp "$APP/Contents/MacOS/MediaConverter" dist/MediaConverter-macos-universal
(cd dist && sha256sum MediaConverter-windows-amd64.exe MediaConverter-macos-universal MediaConverter-Windows.zip MediaConverter-Mac.zip > checksums.txt)
echo "Built Media Converter $VERSION:"
ls -la dist | grep -v '^d'
