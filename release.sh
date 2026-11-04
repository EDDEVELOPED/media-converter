#!/bin/sh
# Builds and publishes a release with the GitHub CLI.
set -e
cd "$(dirname "$0")"
VERSION="$(cat VERSION)"
REPO="EDDEVELOPED/media-converter"
./build.sh
gh release create "v$VERSION" --repo "$REPO" --title "Media Converter $VERSION" --notes-file release-notes.md \
  dist/MediaConverter-Windows.zip dist/MediaConverter-Mac.zip \
  dist/MediaConverter-windows-amd64.exe dist/MediaConverter-macos-universal dist/checksums.txt
echo "Published v$VERSION. Installed copies will offer the update next time they open."
