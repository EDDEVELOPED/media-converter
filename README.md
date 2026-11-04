# FFmpeg GUI - Media Converter

A free, simple FFmpeg GUI for Windows and Mac: a video converter and audio converter you use by dragging in files and clicking Convert, with no command line. It converts to MP4, MOV, MKV, WebM, AVI, GIF, MP3, WAV, FLAC and more, and it also opens Dahua/DVR `.dav` security camera recordings, so you can convert DAV to MP4.

It needs no Python or other runtime: it's a single Go program that shows its interface in an app-style browser window and runs FFmpeg on your computer. It installs FFmpeg for you if you don't have it. Until version 1.3.0 it was called Media Converter.

## Download
- **Windows:** [FFmpeg-GUI-Media-Converter-Windows.zip](https://github.com/EDDEVELOPED/ffmpeg-gui/releases/latest/download/FFmpeg-GUI-Media-Converter-Windows.zip)
- **Mac** (Apple Silicon and Intel): [FFmpeg-GUI-Media-Converter-Mac.zip](https://github.com/EDDEVELOPED/ffmpeg-gui/releases/latest/download/FFmpeg-GUI-Media-Converter-Mac.zip)

Each zip has a "Read me first" file with the one-time steps for opening an app that isn't from a known publisher. After that, the app installs FFmpeg for you and keeps itself up to date.

## Formats
**Opens:** anything FFmpeg can read, including phone and camera video, Dahua/DVR `.dav` recordings (also with unusual export headers) and raw `.264`/`.265` camera streams. If a file doesn't open normally, the app retries with the formats FFmpeg can't always detect on its own (Dahua DHAV, raw H.264/H.265) and with a deeper scan.

**Converts to:**
- Video: MP4 (H.264), MP4 (HEVC), WebM (VP9), MOV, MKV, GIF, plus AVI, WMV, FLV, MPEG-2, TS, 3GP, OGV under More
- Audio: MP3, M4A (AAC), WAV, FLAC, OGG (Opus), plus AAC, WMA, AIFF, Opus, AC3, ALAC under More

On Windows you can also right-click a file and choose **Open with > FFmpeg GUI - Media Converter**, or drag files onto the app's icon.

Options: quality (including "Original", a fast copy with no re-encoding for MP4/MOV/MKV/TS), video size (1080p/720p/480p, never upscales, works with portrait phone video), remove sound, trim start/end per file, save next to the original or in another folder. Existing files are never overwritten.

## FFmpeg and updates
If FFmpeg isn't installed, the **Install FFmpeg** button downloads an official static build (gyan.dev on Windows, with BtbN as a backup; Martin Riedl's builds on Apple Silicon Macs, evermeet.cx on Intel Macs), checks its SHA-256 where the site publishes one, and keeps it in the app's own settings folder. When the app opens it checks that source for a newer FFmpeg (only for the copy it installed; Homebrew or winget copies are left to those tools).

It also checks this repo's releases. If there's a newer one with a file for that computer, the app offers to update, downloads the file, checks it against `checksums.txt`, makes sure it runs, swaps it in and restarts. The open window reconnects to the new version by itself. Drafts, pre-releases and releases without files are skipped.

## Files
- `main.go` startup, single-instance check, opening files passed by "Open with", local server
- `app.go` file queue, conversion runner, HTTP API
- `ffmpeg.go` formats, finding FFmpeg, reading files (including camera recordings), building FFmpeg commands
- `updates.go` one-click FFmpeg install, FFmpeg updates, app self-update
- `platform_*.go` file pickers, Show in Finder/Explorer, opening the window (per OS)
- `ui/index.html` the whole interface (embedded into the program)
- `packaging/` icons, Mac Info.plist, Windows file details (`app.rc`, compiled into `rsrc_windows_amd64.syso`), read-me files
- `build.sh` builds the downloads and update files into `dist/`
- `.github/workflows/release.yml` builds and publishes releases on GitHub

## Tests
`tests/make_dav.py` builds Dahua `.dav` test recordings (following FFmpeg's DHAV reader); `tests/run_tests.py` converts sample files, including those, to every format through the app's API; `tests/update_tests.py` runs the FFmpeg install and update flows and the app self-update against local stand-in servers; `tests/shots.py` takes screenshots of the interface. They need Python 3, Playwright and FFmpeg, and a Linux build of the app (`go build -o /tmp/mc-linux .`).

## Building
Needs Go 1.24+ and Python 3. Run `./build.sh` (output in `dist/`). To add a format, add it to the `formats` list and to `buildArgs` in `ffmpeg.go`.

After changing `packaging/app.rc` or `packaging/app.ico`, rebuild the Windows resource file (needs MinGW's windres):
`cd packaging && x86_64-w64-mingw32-windres --preprocessor=cat app.rc -O coff -o ../rsrc_windows_amd64.syso`

Settings, a log file and the FFmpeg the app installs are kept in `%AppData%\FFmpeg GUI - Media Converter` (Windows) or `~/Library/Application Support/FFmpeg GUI - Media Converter` (Mac). Versions before 1.3.0 used a `MediaConverter` folder; the app moves it to the new name by itself.
