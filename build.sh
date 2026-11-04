#!/bin/sh
# Builds the Windows and Mac downloads plus the files the in-app updater uses.
# Output goes to dist/. The version comes from the VERSION file (or $VERSION).
set -e
cd "$(dirname "$0")"
VERSION="${VERSION:-$(cat VERSION)}"
rm -rf dist && mkdir -p dist/win dist/mac
export CGO_ENABLED=0
LD="-s -w -X main.appVersion=$VERSION"

GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LD -H windowsgui" -o "dist/win/FFmpeg GUI - Media Converter.exe" .
cp "packaging/Read me first (Windows).txt" dist/win/

GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LD" -o dist/mc-arm64 .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$LD" -o dist/mc-amd64 .
APP="dist/mac/FFmpeg GUI - Media Converter.app"
EXE="$APP/Contents/MacOS/FFmpeg GUI - Media Converter"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
python3 packaging/lipo.py "$EXE" dist/mc-amd64 dist/mc-arm64
chmod 755 "$EXE"
sed "s/__VERSION__/$VERSION/g" packaging/Info.plist > "$APP/Contents/Info.plist"
printf 'APPL????' > "$APP/Contents/PkgInfo"
cp packaging/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
cp "packaging/Read me first (Mac).txt" dist/mac/
rm dist/mc-arm64 dist/mc-amd64

# Downloads for people installing for the first time
(cd dist/win && zip -q -X -r ../FFmpeg-GUI-Media-Converter-Windows.zip .)
(cd dist/mac && zip -q -X -r -y ../FFmpeg-GUI-Media-Converter-Mac.zip .)
# What the app downloads when it updates itself
cp "dist/win/FFmpeg GUI - Media Converter.exe" dist/FFmpeg-GUI-Media-Converter-windows-amd64.exe
cp "$EXE" dist/FFmpeg-GUI-Media-Converter-macos-universal
# The same files under their old names, which copies before 1.3.0 (called
# Media Converter) look for. Keep these, or those copies stop getting updates.
cp dist/FFmpeg-GUI-Media-Converter-windows-amd64.exe dist/MediaConverter-windows-amd64.exe
cp dist/FFmpeg-GUI-Media-Converter-macos-universal dist/MediaConverter-macos-universal
(cd dist && sha256sum FFmpeg-GUI-Media-Converter-windows-amd64.exe FFmpeg-GUI-Media-Converter-macos-universal FFmpeg-GUI-Media-Converter-Windows.zip FFmpeg-GUI-Media-Converter-Mac.zip \
  MediaConverter-windows-amd64.exe MediaConverter-macos-universal > checksums.txt)
echo "Built FFmpeg GUI - Media Converter $VERSION:"
ls -la dist | grep -v '^d'
