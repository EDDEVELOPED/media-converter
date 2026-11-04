#!/bin/sh
# Builds and publishes a release with the GitHub CLI.
set -e
cd "$(dirname "$0")"
VERSION="$(cat VERSION)"
REPO="EDDEVELOPED/ffmpeg-gui"
./build.sh
gh release create "v$VERSION" --repo "$REPO" --title "FFmpeg GUI - Media Converter $VERSION" --notes-file release-notes.md \
  dist/FFmpeg-GUI-Media-Converter-Windows.zip dist/FFmpeg-GUI-Media-Converter-Mac.zip \
  dist/FFmpeg-GUI-Media-Converter-windows-amd64.exe dist/FFmpeg-GUI-Media-Converter-macos-universal \
  dist/MediaConverter-windows-amd64.exe dist/MediaConverter-macos-universal dist/checksums.txt
echo "Published v$VERSION. Installed copies will offer the update next time they open."
