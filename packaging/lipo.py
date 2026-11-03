"""Combine thin 64-bit Mach-O binaries into one universal (fat) binary, like Apple's lipo."""
import struct, sys

out, inputs = sys.argv[1], sys.argv[2:]
ALIGN = 14  # 16 KB, what Apple's lipo uses for arm64
slices = []
for path in inputs:
    data = open(path, "rb").read()
    magic, cputype, cpusub = struct.unpack("<III", data[:12])
    assert magic == 0xFEEDFACF, f"{path} is not a 64-bit Mach-O"
    slices.append((cputype, cpusub, data))

header = struct.pack(">II", 0xCAFEBABE, len(slices))
offset = 1 << ALIGN
entries, body = b"", b""
pos = offset
layout = []
for cputype, cpusub, data in slices:
    pos = (pos + (1 << ALIGN) - 1) & ~((1 << ALIGN) - 1)
    entries += struct.pack(">IIIII", cputype, cpusub, pos, len(data), ALIGN)
    layout.append((pos, data))
    pos += len(data)

buf = bytearray(pos)
head = header + entries
buf[: len(head)] = head
for p, data in layout:
    buf[p : p + len(data)] = data
open(out, "wb").write(bytes(buf))

# Check: each slice should carry a code signature load command (LC_CODE_SIGNATURE = 0x1d).
for (cputype, _, data) in slices:
    ncmds = struct.unpack("<I", data[16:20])[0]
    off, signed = 32, False
    for _ in range(ncmds):
        cmd, size = struct.unpack("<II", data[off : off + 8])
        signed |= cmd == 0x1D
        off += size
    arch = {0x01000007: "x86_64", 0x0100000C: "arm64"}.get(cputype, hex(cputype))
    print(f"{arch}: {len(data)} bytes, code signature: {'yes' if signed else 'no'}")
print("wrote", out)
