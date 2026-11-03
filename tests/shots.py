"""Put the app into a realistic mixed state and screenshot the UI."""
import json, os, shutil, subprocess, sys, time, urllib.request
from playwright.sync_api import sync_playwright

BIN = "/tmp/mc-linux"
IN = "/tmp/mctest/in"
OUT = "/tmp/mctest/shots-out"
SHOTS = "/home/claude/tests/shots"
os.makedirs(SHOTS, exist_ok=True)

def start(cfg, extra=None):
    shutil.rmtree(cfg, ignore_errors=True); os.makedirs(cfg)
    shutil.rmtree(OUT, ignore_errors=True); os.makedirs(OUT)
    json.dump({"format": "mp4", "quality": "balanced", "resolution": "original", "saveMode": "folder", "outputDir": OUT}, open(f"{cfg}/settings.json", "w"))
    env = dict(os.environ, MC_NO_BROWSER="1", MC_STAY="1", MC_PRINT_URL="1", MC_CONFIG_DIR=cfg, **(extra or {}))
    p = subprocess.Popen([BIN], env=env, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    url = p.stdout.readline().strip()
    return p, url

def mkcall(url):
    base, token = url.split("/?t=")
    def call(path, body=None, method="POST"):
        req = urllib.request.Request(base + "/api/" + path, method=method,
                                     data=None if method == "GET" else json.dumps(body or {}).encode(),
                                     headers={"X-Token": token, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                return json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            return json.loads(e.read() or b"{}")
    return call

def wait(call, pred, timeout=60):
    t0 = time.time()
    while time.time() - t0 < timeout:
        s = call("state", method="GET")
        if pred(s): return s
        time.sleep(0.2)
    raise TimeoutError

proc, url = start("/tmp/mctest/cfg-shots")
call = mkcall(url)
wait(call, lambda s: not s["checking"])
st = lambda **k: call("settings", {"format": "mp4", "quality": "balanced", "resolution": "original", "saveMode": "folder", **k})

call("add-paths", {"paths": [f"{IN}/odd size no audio.mp4"]})
wait(call, lambda s: s["jobs"][-1]["status"] == "ready")
st(format="mp3"); call("start"); wait(call, lambda s: not s["running"])
call("add-paths", {"paths": [f"{IN}/Shop walkaround.mov"]})
wait(call, lambda s: s["jobs"][-1]["status"] == "ready")
st(format="mp4"); call("start"); wait(call, lambda s: not s["running"])
call("add-paths", {"paths": [f"{IN}/notes.mp4", f"{IN}/phone vertical 5.1.mkv", f"{IN}/voicemail.wav"]})
wait(call, lambda s: all(j["status"] not in ("probing",) for j in s["jobs"]))

with sync_playwright() as pw:
    b = pw.chromium.launch()
    for scheme in ["light", "dark"]:
        pg = b.new_page(viewport={"width": 1120, "height": 780}, color_scheme=scheme)
        pg.goto(url)
        pg.wait_for_selector(".job")
        if scheme == "light":
            st(format="webm", quality="high")
            time.sleep(0.3)
            # open trim on the done row before starting
            pg.click("li.job.done button[data-a=trim]")
            call("start")
            wait(call, lambda s: any(j["status"] == "running" and j["progress"] > 0.2 for j in s["jobs"]), timeout=120)
            time.sleep(1.2)
            pg.screenshot(path=f"{SHOTS}/main-light.png")
            call("stop"); wait(call, lambda s: not s["running"])
            st(format="mp4")
        else:
            time.sleep(0.6)
            pg.screenshot(path=f"{SHOTS}/main-dark.png")
        pg.close()

    pg = b.new_page(viewport={"width": 640, "height": 1100})
    pg.goto(url); pg.wait_for_selector(".job"); time.sleep(0.4)
    pg.screenshot(path=f"{SHOTS}/narrow.png", full_page=True)
    pg.close()
    call("quit"); proc.wait(timeout=10)

    # Empty + FFmpeg missing
    proc, url = start("/tmp/mctest/cfg-shots2", {"MC_TEST_NO_FFMPEG": "1"})
    call = mkcall(url)
    for os_name in ["mac"]:
        pg = b.new_page(viewport={"width": 1120, "height": 780})
        pg.goto(url); pg.wait_for_selector("#setup:not([hidden])"); time.sleep(0.3)
        pg.screenshot(path=f"{SHOTS}/setup.png")
        pg.close()
    call("quit"); proc.wait(timeout=10)

    proc, url = start("/tmp/mctest/cfg-shots3")
    call = mkcall(url)
    pg = b.new_page(viewport={"width": 1120, "height": 780})
    pg.goto(url); pg.wait_for_selector("#fmtVideo .tile"); time.sleep(0.5)
    pg.click("text=MP3"); time.sleep(0.4)
    pg.screenshot(path=f"{SHOTS}/empty.png")
    errors = []
    pg.on("pageerror", lambda e: errors.append(str(e)))
    pg.reload(); time.sleep(0.8)
    print("page errors:", errors)
    call("quit"); time.sleep(1.0)
    pg.screenshot(path=f"{SHOTS}/closed.png")
    proc.wait(timeout=10)
    b.close()
print("shots done")
