"""End-to-end test of one-click FFmpeg install, FFmpeg update, and app self-update,
against local stand-ins for the download sites and GitHub releases."""
import hashlib, http.server, json, os, shutil, subprocess, sys, threading, time, urllib.request, zipfile
from playwright.sync_api import sync_playwright

ROOT = "/tmp/mcupd"
SRV, APPDIR, CFG = f"{ROOT}/srv", f"{ROOT}/app", f"{ROOT}/cfg"
SHOTS = "/home/claude/tests/shots"
SRC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
shutil.rmtree(ROOT, ignore_errors=True)
for d in (f"{SRV}/ff", f"{SRV}/rel", APPDIR, CFG, SHOTS):
    os.makedirs(d, exist_ok=True)
PORT = 8810
BASE = f"http://127.0.0.1:{PORT}"

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

# --- builds: the "installed" old version and the "published" new one
env = dict(os.environ, CGO_ENABLED="0")
subprocess.run(["go", "build", "-ldflags", "-X main.appVersion=1.0.0", "-o", f"{APPDIR}/mc", "."], cwd=SRC, env=env, check=True)
subprocess.run(["go", "build", "-ldflags", "-X main.appVersion=1.2.0", "-o", f"{SRV}/rel/FFmpeg-GUI-Media-Converter-linux-amd64", "."], cwd=SRC, env=env, check=True)
asset = f"{SRV}/rel/FFmpeg-GUI-Media-Converter-linux-amd64"
open(f"{SRV}/rel/checksums.txt", "w").write(f"{sha(asset)}  FFmpeg-GUI-Media-Converter-linux-amd64\n")
json.dump({"tag_name": "v1.2.0", "html_url": "https://github.com/EDDEVELOPED/ffmpeg-gui/releases/tag/v1.2.0",
           "body": "Faster GIFs and a clearer progress bar.\n\nAlso fixes trimming very short clips.", "draft": False, "prerelease": False,
           "assets": [{"name": "FFmpeg-GUI-Media-Converter-linux-amd64", "browser_download_url": f"{BASE}/rel/FFmpeg-GUI-Media-Converter-linux-amd64", "size": os.path.getsize(asset)},
                      {"name": "checksums.txt", "browser_download_url": f"{BASE}/rel/checksums.txt", "size": 100}]},
          open(f"{SRV}/rel/latest.json", "w"))

# --- a fake FFmpeg download: real ffmpeg inside bin/, padded so progress is visible
z = f"{SRV}/ff/ffmpeg.zip"
with zipfile.ZipFile(z, "w") as zf:
    zf.write("/usr/bin/ffmpeg", "ffmpeg-6.1-test/bin/ffmpeg", compress_type=zipfile.ZIP_DEFLATED)
    zf.writestr(zipfile.ZipInfo("ffmpeg-6.1-test/doc/padding.bin"), os.urandom(14 << 20), compress_type=zipfile.ZIP_STORED)
open(z + ".sha256", "w").write(sha(z))
open(z + ".ver", "w").write("6.1.1\n")

class Slow(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **k): super().__init__(*a, directory=SRV, **k)
    def log_message(self, *a): pass
    def copyfile(self, src, dst):
        slow = self.path.endswith(".zip") or "linux-amd64" in self.path
        while True:
            buf = src.read(256 << 10)
            if not buf: break
            dst.write(buf)
            if slow: time.sleep(0.07)  # about 3.5 MB/s
srv = http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Slow)
threading.Thread(target=srv.serve_forever, daemon=True).start()

def launch(update_api):
    e = dict(os.environ, MC_CONFIG_DIR=CFG, MC_TEST_NO_FFMPEG="1", MC_TEST_FFMPEG_URL=f"{BASE}/ff/ffmpeg.zip",
             MC_UPDATE_API=update_api, MC_NO_BROWSER="1", MC_PRINT_URL="1", MC_STAY="1")
    p = subprocess.Popen([f"{APPDIR}/mc"], env=e, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    return p, p.stdout.readline().strip()

def api(url, path, method="POST"):
    base, token = url.split("/?t=")
    req = urllib.request.Request(base + "/api/" + path, method=method, data=None if method == "GET" else b"{}",
                                 headers={"X-Token": token, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or b"{}")

def check(ok, label):
    print(("PASS  " if ok else "FAIL  ") + label)

with sync_playwright() as pw:
    br = pw.chromium.launch()

    # 1) No FFmpeg: one-click install
    proc, url = launch(f"{BASE}/rel/missing.json")
    pg = br.new_page(viewport={"width": 1120, "height": 780}, bypass_csp=True)
    pg.goto(url)
    pg.wait_for_selector("#setup:not([hidden]) #ffInstall")
    time.sleep(0.4)
    pg.screenshot(path=f"{SHOTS}/install-1-setup.png")
    pg.click("#ffInstall")
    pg.wait_for_function("document.querySelector('#installText').textContent.startsWith('Downloading')", timeout=15000)
    time.sleep(1.6)
    pg.screenshot(path=f"{SHOTS}/install-2-progress.png")
    pg.wait_for_function("document.querySelector('#ffPill').textContent.includes('FFmpeg 6')", timeout=60000)
    time.sleep(0.5)
    pg.screenshot(path=f"{SHOTS}/install-3-done.png")
    st = api(url, "state", "GET")
    check(st["ffmpeg"]["found"] and st["ffmpeg"]["path"] == f"{CFG}/ffmpeg/ffmpeg", f"FFmpeg installed to app folder ({st['ffmpeg']['path']})")
    check(json.load(open(f"{CFG}/ffmpeg/installed.json"))["version"] == "6.1.1", "installed.json records 6.1.1")
    check("FFmpeg is installed" in pg.inner_text("#toast"), "toast confirms install")
    check(not os.path.exists(f"{CFG}/ffmpeg/download.zip"), "download zip cleaned up")
    # Cancel mid-download, then the bad-checksum path
    api(url, "ffmpeg/install"); time.sleep(1.5); api(url, "ffmpeg/cancel"); time.sleep(0.8)
    st = api(url, "state", "GET")
    check(not st["install"]["active"] and st["ffmpeg"]["found"], "cancelling a re-install keeps the working FFmpeg")
    good = open(z + ".sha256").read()
    open(z + ".sha256", "w").write("0" * 64)
    api(url, "ffmpeg/install")
    for _ in range(100):
        st = api(url, "state", "GET")
        if not st["install"]["active"]: break
        time.sleep(0.2)
    check(st["install"]["phase"] == "failed" and "checksum" in st["install"]["error"], "damaged download is rejected: " + st["install"]["error"][:90])
    open(z + ".sha256", "w").write(good)
    api(url, "quit"); proc.wait(timeout=10); pg.close()

    # 2) Next launch: a newer FFmpeg and a newer app are both published
    open(z + ".ver", "w").write("6.2\n")
    proc, url = launch(f"{BASE}/rel/latest.json")
    pg = br.new_page(viewport={"width": 1120, "height": 780}, bypass_csp=True)
    pg.goto(url)
    pg.wait_for_function("document.querySelectorAll('.banner').length == 2", timeout=20000)
    time.sleep(0.4)
    pg.screenshot(path=f"{SHOTS}/update-1-banners.png")
    banners = pg.inner_text("#banners")
    check("Media Converter 1.2.0 is available" in banners and "FFmpeg 6.2 is available" in banners, "both update offers shown")

    pg.click("button[data-do=ff-update]")
    pg.wait_for_function("document.querySelector('#toast').textContent.includes('FFmpeg is updated')", timeout=60000)
    check(json.load(open(f"{CFG}/ffmpeg/installed.json"))["version"] == "6.2", "FFmpeg update recorded as 6.2")
    check("FFmpeg 6.2" not in pg.inner_text("#banners"), "FFmpeg banner gone after update")

    old_pid = proc.pid
    pg.click("button[data-do=app-update]")
    pg.wait_for_function("document.querySelector('#banners').textContent.includes('Downloading')", timeout=10000)
    time.sleep(0.8)
    pg.screenshot(path=f"{SHOTS}/update-2-downloading.png")
    pg.wait_for_function("document.querySelector('#versionBtn').textContent == 'Version 1.2.0'", timeout=60000)
    time.sleep(0.5)
    pg.screenshot(path=f"{SHOTS}/update-3-updated.png")
    check("Updated to Media Converter 1.2.0" in pg.inner_text("#toast"), "window reconnected to the new version by itself")
    proc.wait(timeout=10)
    out = subprocess.run([f"{APPDIR}/mc", "--version"], capture_output=True, text=True).stdout.strip()
    check(out == "1.2.0", f"app file on disk is now {out}")
    time.sleep(1)
    check(not os.path.exists(f"{APPDIR}/mc.new") and not os.path.exists(f"{APPDIR}/mc.old"), "no leftover .new/.old files")
    inst = json.load(open(f"{CFG}/instance.json"))
    check(inst["pid"] != old_pid, "instance file belongs to the new process")
    st = api(url, "state", "GET")
    check(st["ffmpeg"]["found"] and st["appUpdate"] is None, "new version sees FFmpeg and no further update")

    pg.click("#versionBtn")
    pg.wait_for_function("document.querySelector('#toast').textContent.includes('latest version')", timeout=20000)
    check(True, "Check for updates says up to date")
    api(url, "quit")
    time.sleep(1)
    br.close()
srv.shutdown()
print("done")
