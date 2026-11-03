"""Drive the Media Converter API end to end and check every output with ffprobe."""
import json, os, shutil, subprocess, sys, time, urllib.request

BIN = sys.argv[1]
ROOT = "/tmp/mctest"
IN, OUT, CFG = f"{ROOT}/in", f"{ROOT}/out", f"{ROOT}/cfg"
shutil.rmtree(OUT, ignore_errors=True); os.makedirs(OUT)
shutil.rmtree(CFG, ignore_errors=True); os.makedirs(CFG)
json.dump({"format": "mp4", "quality": "balanced", "resolution": "original", "saveMode": "folder", "outputDir": OUT},
          open(f"{CFG}/settings.json", "w"))

env = dict(os.environ, MC_NO_BROWSER="1", MC_STAY="1", MC_PRINT_URL="1", MC_CONFIG_DIR=CFG)
proc = subprocess.Popen([BIN], env=env, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
url = proc.stdout.readline().strip()
base, token = url.split("/?t=")
print("server", base)

def call(path, body=None, method="POST"):
    req = urllib.request.Request(base + "/api/" + path, method=method,
                                 data=None if method == "GET" else json.dumps(body or {}).encode(),
                                 headers={"X-Token": token, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")

def state():
    return call("state", method="GET")[1]

def wait(pred, timeout=120):
    t0 = time.time()
    while time.time() - t0 < timeout:
        s = state()
        if pred(s):
            return s
        time.sleep(0.25)
    raise TimeoutError("timed out")

wait(lambda s: not s["checking"])
s = state()
assert s["ffmpeg"]["found"], s["ffmpeg"]
print("ffmpeg", s["ffmpeg"]["version"], "| unavailable formats:", [f["id"] for f in s["formats"] if not f["available"]])

def probe(path):
    out = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration,format_name:stream=codec_type,codec_name,width,height,channels,codec_tag_string",
                          "-of", "json", path], capture_output=True, text=True)
    return json.loads(out.stdout)

def run(name, files, settings, trim=None, expect="done"):
    call("stop"); call("clear")
    for j in state()["jobs"]:
        call("remove", {"id": j["id"]})
    call("add-paths", {"paths": [f"{IN}/{f}" for f in files]})
    s = wait(lambda s: s["jobs"] and all(j["status"] not in ("probing",) for j in s["jobs"]))
    st = {"format": "mp4", "quality": "balanced", "resolution": "original", "removeAudio": False, "saveMode": "folder"}
    st.update(settings)
    code, _ = call("settings", st)
    assert code == 200
    if trim:
        for j in s["jobs"]:
            call("job", {"id": j["id"], "trimStart": trim[0], "trimEnd": trim[1]})
    ready = [j for j in state()["jobs"] if j["status"] == "ready"]
    if ready:
        code, resp = call("start")
        assert code == 200, resp
        s = wait(lambda s: not s["running"], timeout=300)
    else:
        s = state()
    results = []
    for j in s["jobs"]:
        ok = j["status"] == expect
        info = ""
        if j["status"] == "done":
            p = probe(j["output"])
            streams = [(x["codec_type"], x["codec_name"], x.get("width"), x.get("height"), x.get("channels"), x.get("codec_tag_string")) for x in p["streams"]]
            info = f"{float(p['format'].get('duration', 0) or 0):.2f}s {streams}"
        else:
            info = j["error"] or j["status"]
        results.append(ok)
        print(f"{'PASS' if ok else 'FAIL'}  {name:34s} {j['name'][:26]:26s} -> {j['status']:8s} {j.get('outName','')}  {info}")
    return s

run("mp4 balanced", ["Shop walkaround.mov"], {"format": "mp4"})
run("hevc small 720p", ["Shop walkaround.mov"], {"format": "hevc", "quality": "small", "resolution": "720"})
run("webm small", ["Shop walkaround.mov"], {"format": "webm", "quality": "small"})
run("mov copy", ["Shop walkaround.mov"], {"format": "mov", "quality": "copy"})
run("mkv copy", ["Shop walkaround.mov"], {"format": "mkv", "quality": "copy"})
run("mp4 copy keeps pcm audio", ["pcm audio.mov"], {"format": "mp4", "quality": "copy"})
run("gif medium", ["Shop walkaround.mov"], {"format": "gif"})
run("portrait 720p", ["phone vertical 5.1.mkv"], {"format": "mp4", "resolution": "720"})
run("portrait webm 5.1->opus", ["phone vertical 5.1.mkv"], {"format": "webm", "quality": "small"})
run("odd size mp4 no audio", ["odd size no audio.mp4"], {"format": "mp4"})
run("remove sound", ["Shop walkaround.mov"], {"format": "mp4", "removeAudio": True})
for fmt in ["mp3", "m4a", "wav", "flac", "ogg"]:
    run(f"audio {fmt} from wav", ["voicemail.wav"], {"format": fmt})
run("ogg from 5.1 video", ["phone vertical 5.1.mkv"], {"format": "ogg"})
run("mp3 from video", ["Shop walkaround.mov"], {"format": "mp3", "quality": "high"})
run("trim 0:02-0:05 armenian name", ["Սարքավորում test.mov"], {"format": "mp4"}, trim=("0:02", "0:05"))
run("audio-only file to mp4 fails", ["voicemail.wav"], {"format": "mp4"}, expect="failed")
run("no-sound video to mp3 fails", ["odd size no audio.mp4"], {"format": "mp3"}, expect="failed")
run("not a video", ["notes.mp4"], {"format": "mp4"}, expect="invalid")
st = run("empty .dav (0 bytes)", ["empty-export.dav"], {"format": "mp4"}, expect="invalid")
print(f"{'PASS' if 'empty (0 bytes)' in st['jobs'][0]['error'] else 'FAIL'}  empty file explains itself")
# Same name again: must not overwrite, should become "(converted)"
run("name clash", ["Shop walkaround.mov"], {"format": "mp4"})

# ---- Security-camera recordings and raw streams
for f in ["cam1 plain.dav", "cam2 dahua header.dav", "cam3 export header.dav", "cam4 h265.dav", "dvr raw.264"]:
    st = run(f"camera file -> mp4", [f], {"format": "mp4"})
    j = st["jobs"][0]
    print(f"      container={j['container']} fps={j['fps']} audio={j['hasAudio']} duration={j['duration']}")
st = run("dav unknown header keeps sound+fps", ["cam3 export header.dav"], {"format": "mp4"})
p = probe(st["jobs"][0]["output"])
v = [x for x in p["streams"] if x["codec_type"] == "video"]
fps_out = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "stream=r_frame_rate", "-of", "csv=p=0", st["jobs"][0]["output"]], capture_output=True, text=True).stdout.strip()
ok = any(x["codec_type"] == "audio" for x in p["streams"]) and fps_out == "20/1"
print(f"{'PASS' if ok else 'FAIL'}  unknown-header .dav: audio kept, {fps_out} fps")
run("dav 15fps -> MPEG-2", ["cam2 dahua header.dav"], {"format": "mpg"})
st = run("dav trim 0:01-0:03", ["cam1 plain.dav"], {"format": "mp4"}, trim=("0:01", "0:03"))
d = float(probe(st["jobs"][0]["output"])["format"].get("duration", 0) or 0)
print(f"{'PASS' if 1.8 < d < 2.3 else 'FAIL'}  trimmed .dav is {d:.2f}s long (expected 2s)")
st = run("raw .264 trim 0:01-0:03", ["dvr raw.264"], {"format": "mp4"}, trim=("0:01", "0:03"))
d = float(probe(st["jobs"][0]["output"])["format"].get("duration", 0) or 0)
print(f"{'PASS' if 1.8 < d < 2.3 else 'FAIL'}  trimmed raw stream is {d:.2f}s long (expected 2s)")
run("5.1 source -> wmv", ["phone vertical 5.1.mkv"], {"format": "wmv"})
run("5.1 source -> 3gp", ["phone vertical 5.1.mkv"], {"format": "3gp"})
run("dav -> mp3 (sound only)", ["cam1 plain.dav"], {"format": "mp3"})

# ---- More output formats
for fmt in ["avi", "wmv", "flv", "mpg", "ts", "3gp", "ogv"]:
    run(f"video {fmt}", ["Shop walkaround.mov"], {"format": fmt})
run("ts copy", ["Shop walkaround.mov"], {"format": "ts", "quality": "copy"})
for fmt in ["aac", "wma", "aiff", "opus", "ac3", "alac"]:
    run(f"audio {fmt}", ["voicemail.wav"], {"format": fmt})
for fmt in ["mp3", "wma", "ac3", "mpg", "avi"]:
    run(f"5.1 source -> {fmt}", ["phone vertical 5.1.mkv"], {"format": fmt})

# Trim validation
call("clear")
for j in state()["jobs"]:
    call("remove", {"id": j["id"]})
call("add-paths", {"paths": [f"{IN}/Shop walkaround.mov"]})
s = wait(lambda s: s["jobs"] and s["jobs"][0]["status"] == "ready")
jid = s["jobs"][0]["id"]
for a, b in [("0:05", "0:02"), ("abc", ""), ("0:30", ""), ("1:75", "")]:
    _, r = call("job", {"id": jid, "trimStart": a, "trimEnd": b})
    print(f"{'PASS' if r['trimError'] else 'FAIL'}  trim check {a!r}-{b!r}: {r['trimError']}")
call("job", {"id": jid, "trimStart": "", "trimEnd": ""})

# Cancel mid-way: slow webm high on the 1080p clip
call("settings", {"format": "webm", "quality": "high", "resolution": "original", "saveMode": "folder"})
before = set(os.listdir(OUT))
call("start")
wait(lambda s: s["jobs"][0]["status"] == "running" and s["jobs"][0]["progress"] > 0.05, timeout=60)
mid = state()["jobs"][0]
print("      progress sample:", round(mid["progress"], 3), mid["speed"], round(mid["eta"], 1))
call("stop")
s = wait(lambda s: not s["running"])
after = set(os.listdir(OUT))
print(f"{'PASS' if s['jobs'][0]['status']=='canceled' and after==before else 'FAIL'}  cancel removes partial file  ({s['jobs'][0]['status']}, new files: {after-before})")

# Security: no token / wrong host
req = urllib.request.Request(base + "/api/state")
try:
    urllib.request.urlopen(req); print("FAIL  no-token request allowed")
except urllib.error.HTTPError as e:
    print(f"{'PASS' if e.code==401 else 'FAIL'}  no-token request rejected ({e.code})")
req = urllib.request.Request(base + "/api/state", headers={"X-Token": token, "Host": "evil.example:80"})
try:
    urllib.request.urlopen(req); print("FAIL  wrong host allowed")
except urllib.error.HTTPError as e:
    print(f"{'PASS' if e.code==403 else 'FAIL'}  wrong host rejected ({e.code})")

print("outputs:", sorted(os.listdir(OUT)))
call("quit")
proc.wait(timeout=10)
print("server exited", proc.returncode)
