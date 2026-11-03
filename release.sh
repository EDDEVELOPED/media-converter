#!/bin/sh
# Publishes a new version: bump VERSION, write release-notes.md, then run ./release.sh
# Needs the GitHub CLI (gh) signed in to an account that can push to the repo.
set -e
cd "$(dirname "$0")"
VERSION="$(cat VERSION)"
REPO="EDDEVELOPED/media-converter"
./build.sh
gh release create "v$VERSION" --repo "$REPO" --title "Media Converter $VERSION" --notes-file release-notes.md \
  dist/MediaConverter-Windows.zip dist/MediaConverter-Mac.zip \
  dist/MediaConverter-windows-amd64.exe dist/MediaConverter-macos-universal dist/checksums.txt
echo "Published v$VERSION. Installed copies will offer the update next time they open."
