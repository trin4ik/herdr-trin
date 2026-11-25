"""Return only bounded PNG image data from the selected project folders."""
import base64
import errno
import json
import os
import stat
import struct
import sys
import zlib

MAX_BYTES = 160 * 1024
MAX_EDGE = 512
SIGNATURE = b"\x89PNG\r\n\x1a\n"


def clean_png(raw):
    if not raw.startswith(SIGNATURE) or len(raw) > MAX_BYTES:
        return None
    pos, parts, compressed = len(SIGNATURE), [SIGNATURE], []
    palette, transparency, data_closed = 0, False, False
    while pos + 12 <= len(raw):
        length, kind = struct.unpack_from(">I4s", raw, pos)
        end = pos + 12 + length
        if end > len(raw) or zlib.crc32(raw[pos + 4:end - 4]) != int.from_bytes(raw[end - 4:end], "big"):
            return None
        payload = raw[pos + 8:end - 4]
        if pos == 8:
            if kind != b"IHDR" or length != 13:
                return None
            width, height = struct.unpack_from(">II", payload)
            if not (1 <= width == height <= MAX_EDGE):
                return None
            depth, color, compression, filtering, interlace = payload[8:]
            channels = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}.get(color)
            depths = {0: (1, 2, 4, 8, 16), 2: (8, 16), 3: (1, 2, 4, 8), 4: (8, 16), 6: (8, 16)}
            if not channels or depth not in depths[color] or compression or filtering or interlace:
                return None
            stride = 1 + (width * channels * depth + 7) // 8
        elif kind == b"IHDR":
            return None
        if kind == b"PLTE":
            if palette or transparency or compressed or color in (0, 4) or not (0 < length <= 768) or length % 3:
                return None
            palette = length // 3
            if color == 3 and palette > 1 << depth:
                return None
        if kind == b"tRNS":
            if transparency or compressed or not (
                (color == 0 and length == 2) or (color == 2 and length == 6) or (color == 3 and 0 < length <= palette)
            ):
                return None
            transparency = True
        if kind in (b"IHDR", b"PLTE", b"tRNS", b"IDAT", b"IEND"):
            parts.append(raw[pos:end])
        elif not (kind[0] & 32):
            return None
        if kind == b"IDAT":
            if data_closed or (color == 3 and not palette):
                return None
            compressed.append(payload)
        elif compressed:
            data_closed = True
        if kind == b"IEND":
            if not compressed or length or end != len(raw):
                return None
            try:
                decoder = zlib.decompressobj()
                expected = height * stride
                pixels = decoder.decompress(b"".join(compressed), expected + 1)
                if not decoder.eof or decoder.unused_data or len(pixels) != expected or any(x > 4 for x in pixels[::stride]):
                    return None
            except zlib.error:
                return None
            return b"".join(parts)
        pos = end
    return None


def icon(folder):
    # A repo cannot use symlinks, fifos or devices to turn icon discovery
    # into a read of an arbitrary file. The selected root itself may be a link.
    if not isinstance(folder, str) or not os.path.isabs(folder) or "\x00" in folder:
        return None
    try:
        root = os.open(folder, os.O_RDONLY | os.O_DIRECTORY)
    except OSError as error:
        return "" if error.errno in (errno.ENOENT, errno.ENOTDIR) else None
    directory = fd = None
    try:
        directory = os.open(".herdr", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root)
        fd = os.open("icon.png", os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW, dir_fd=directory)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= MAX_BYTES:
            return ""
        with os.fdopen(fd, "rb", closefd=False) as file:
            png = clean_png(file.read(MAX_BYTES + 1))
        return base64.b64encode(png).decode("ascii") if png else ""
    except OSError as error:
        return "" if error.errno in (errno.ENOENT, errno.ENOTDIR, errno.ELOOP) else None
    finally:
        for handle in (fd, directory, root):
            if handle is not None:
                os.close(handle)


if __name__ == "__main__":
    folders = json.loads(sys.argv[1])
    print(json.dumps([icon(folder) for folder in folders[:8]]))
