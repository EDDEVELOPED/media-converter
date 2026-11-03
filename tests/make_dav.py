"""Build Dahua .dav (DHAV) test files, following FFmpeg's libavformat/dhav.c reader.

Packet: "DHAV" type subtype channel subnum | frame_no u32 | length u32 | date u32 |
        ms u16 | ext_len u8 | checksum u8 | ext... | payload | "dhav" length u32
"""
import os, struct, subprocess, sys

OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/mctest/in"
os.makedirs(OUT, exist_ok=True)

def date_bits(sec_total):
    # 2026-11-03 09:00:00 plus sec_total seconds (stays inside the same hour for short clips)
    s = sec_total % 60; m = (sec_total // 60) % 60
    return s | (m << 6) | (9 << 12) | (3 << 17) | (11 << 22) | ((2026 - 2000) << 26)

def packet(ptype, frame_no, t_ms, ext, payload):
    length = 24 + len(ext) + len(payload) + 8
    head = b"DHAV" + bytes([ptype, 0, 0, 0]) + struct.pack("<III", frame_no, length, date_bits(t_ms // 1000))
    head += struct.pack("<HBB", t_ms % 65536, len(ext), 0)
    return head + ext + payload + b"dhav" + struct.pack("<I", length)

def encode(codec, w, h, fps, secs):
    """Raw Annex-B video with an access-unit delimiter before every frame."""
    if codec == "h264":
        args = ["-c:v", "libx264", "-x264-params", "aud=1:keyint=%d" % (fps * 2), "-f", "h264"]
        aud = b"\x00\x00\x00\x01\x09"
    else:
        args = ["-c:v", "libx265", "-x265-params", "aud=1:keyint=%d:log-level=error" % (fps * 2), "-f", "hevc"]
        aud = b"\x00\x00\x00\x01\x46\x01"
    raw = subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", f"testsrc2=size={w}x{h}:rate={fps}", "-t", str(secs),
                          "-pix_fmt", "yuv420p", "-preset", "ultrafast", *args, "-"], capture_output=True, check=True).stdout
    frames = [aud + p for p in raw.split(aud) if p]
    return frames

def is_key(codec, au):
    i = 0
    while True:
        i = au.find(b"\x00\x00\x01", i)
        if i < 0: return False
        nal = au[i + 3]
        if codec == "h264" and (nal & 0x1F) == 5: return True
        if codec == "hevc" and 16 <= ((nal >> 1) & 0x3F) <= 21: return True
        i += 3

def alaw(secs):
    return subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=f=600:sample_rate=8000", "-t", str(secs),
                           "-ac", "1", "-f", "alaw", "-"], capture_output=True, check=True).stdout

def build(name, codec, w, h, fps, secs, prefix=b"", audio=True):
    frames = encode(codec, w, h, fps, secs)
    a = alaw(secs) if audio else b""
    vcode = 0x2 if codec == "h264" else 0xC
    vext = bytes([0x80, 0, w // 8, h // 8]) + bytes([0x81, 0, vcode, fps])
    aext = bytes([0x83, 1, 0x0E, 0])  # 1 channel, G.711 A-law, 8000 Hz
    out = bytearray(prefix)
    apos = 0
    for n, au in enumerate(frames):
        t = n * 1000 // fps
        key = is_key(codec, au)
        out += packet(0xFD if key else 0xFC, n, t, vext if key else b"", au)
        while audio and apos < len(a) and apos * 1000 // 8000 <= t:  # 40 ms audio chunks
            out += packet(0xF0, apos // 320, apos * 1000 // 8000, aext, a[apos:apos + 320])
            apos += 320
    open(os.path.join(OUT, name), "wb").write(out)
    print(f"{name}: {len(out)} bytes, {len(frames)} frames")

build("cam1 plain.dav", "h264", 1280, 720, 25, 6)
dahua_header = b"DAHUA" + b"\x00" * (0x400 - 5)
build("cam2 dahua header.dav", "h264", 704, 480, 15, 5, prefix=dahua_header)
# An export with an unknown file header in front: FFmpeg can't auto-detect this one.
build("cam3 export header.dav", "h264", 1920, 1080, 20, 5, prefix=b"LIDH" + os.urandom(2044))
build("cam4 h265.dav", "hevc", 1280, 720, 15, 5)

# A raw H.264 recording with a proprietary header (common with other DVR brands).
frames = encode("h264", 960, 544, 25, 4)
open(os.path.join(OUT, "dvr raw.264"), "wb").write(b"IMKH" + os.urandom(36) + b"".join(frames))
print("dvr raw.264 written")
