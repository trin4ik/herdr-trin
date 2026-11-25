from pathlib import Path
import base64
import runpy
import struct
import tempfile
import unittest
import zlib

reader = runpy.run_path(str(Path(__file__).parents[1] / "mods/trin-projects/project-icons.py"))
signature = reader["SIGNATURE"]


def chunk(tag, data):
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data))


def png(edge=1, payload=b"\0\xff\0\0\xff"):
    header = struct.pack(">IIBBBBB", edge, edge, 8, 6, 0, 0, 0)
    return (signature + chunk(b"IHDR", header) + chunk(b"tEXt", b"fixture\0must not cross")
            + chunk(b"IDAT", zlib.compress(payload)) + chunk(b"IEND", b""))


class ProjectIconTests(unittest.TestCase):
    def test_reader_strips_text_and_rejects_invalid_or_unbounded_images(self):
        clean = reader["clean_png"]
        self.assertNotIn(b"must not cross", clean(png()))
        for raw in (png(513), png(payload=b"x" * 100000), png()[:-1], png() + b"trailer", b"not an image"):
            self.assertIsNone(clean(raw))
        header = chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 3, 0, 0, 0))
        data = chunk(b"IDAT", zlib.compress(b"\0\0")) + chunk(b"IEND", b"")
        self.assertIsNone(clean(signature + header + data))
        self.assertIsNotNone(clean(signature + header + chunk(b"PLTE", b"\xff\0\0") + data))

    def test_only_the_real_bounded_file_is_read(self):
        icon = reader["icon"]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.assertEqual(icon(str(root / "missing-project")), "")
            config = root / ".herdr"
            self.assertEqual(icon(directory), "")
            config.mkdir()
            dest = config / "icon.png"
            other = root / "some-image.png"
            other.write_bytes(png())
            dest.symlink_to(other)
            self.assertEqual(icon(directory), "")
            dest.unlink()
            dest.write_bytes(png())
            self.assertEqual(base64.b64decode(icon(directory)), reader["clean_png"](png()))
            config.rename(root / ".moved")
            config.symlink_to(root / ".moved", target_is_directory=True)
            self.assertEqual(icon(directory), "")
