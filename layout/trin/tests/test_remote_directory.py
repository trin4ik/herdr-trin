"""Read-only directory protocol: logical paths, symlinks, permissions, paging."""
import errno
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[1] / "mods/trin-projects/remote-directory.py"
spec = importlib.util.spec_from_file_location("remote_directory", SOURCE)
reader = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reader)


class RemoteDirectoryTests(unittest.TestCase):
    def setUp(self):
        base = Path(tempfile.gettempdir()) / "opencode"
        self.temp = tempfile.TemporaryDirectory(prefix="directory-picker-", dir=base if base.exists() else None)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.home.mkdir()
        self.external = self.root / "another disk"
        self.external.mkdir()
        (self.external / "subfolder").mkdir()
        (self.home / "projects").symlink_to(self.external, target_is_directory=True)
        (self.home / "relative").symlink_to("../another disk", target_is_directory=True)

    def read(self, path="", **kwargs):
        with patch.object(reader.os.path, "expanduser", return_value=str(self.home)):
            return reader.read_directory({"path": path, **kwargs})

    def test_home_and_tilde_are_remote_and_keep_logical_symlink_paths(self):
        self.assertEqual(self.read()["path"], str(self.home))
        linked = self.read("~/projects")
        self.assertTrue(linked["ok"])
        self.assertEqual(linked["path"], str(self.home / "projects"))
        self.assertEqual(linked["realPath"], os.path.realpath(self.external))
        self.assertEqual(linked["entries"][0]["name"], "subfolder")

    def test_relative_links_and_links_to_external_roots_are_directories(self):
        entries = {e["name"]: e for e in self.read()["entries"]}
        self.assertTrue(entries["relative"]["link"])
        self.assertEqual(entries["relative"]["target"], "../another disk")
        self.assertTrue(self.read("~/relative/subfolder")["ok"])
        self.assertTrue(self.read(str(self.external))["ok"])

    def test_logical_up_returns_to_visible_parent_not_another_disk(self):
        self.assertEqual(self.read("~/projects/..")["path"], str(self.home))
        self.assertEqual(self.read("..", base=str(self.home / "projects"))["path"], str(self.home))
        self.assertEqual(self.read("/")["path"], "/")

    def test_broken_and_loop_links_are_reported_not_recursively_followed(self):
        (self.home / "broken").symlink_to("missing")
        (self.home / "loop-a").symlink_to("loop-b")
        (self.home / "loop-b").symlink_to("loop-a")
        entries = {e["name"]: e for e in self.read()["entries"]}
        self.assertEqual(entries["broken"]["error"], "not_found")
        self.assertEqual(entries["loop-a"]["error"], "symlink_loop")
        self.assertEqual(self.read("~/loop-a")["code"], "symlink_loop")

    def test_only_folders_are_returned_hidden_is_explicit(self):
        (self.home / "file").write_text("not inspected")
        (self.home / "file-link").symlink_to("file")
        (self.home / ".hidden").mkdir()
        self.assertNotIn("file", [e["name"] for e in self.read()["entries"]])
        self.assertNotIn("file-link", [e["name"] for e in self.read()["entries"]])
        self.assertNotIn(".hidden", [e["name"] for e in self.read()["entries"]])
        self.assertIn(".hidden", [e["name"] for e in self.read(hidden=True)["entries"]])

    def test_page_cursor_and_scan_budget_are_bounded(self):
        for name in ["a", "B", "c", "d"]:
            (self.home / name).mkdir()
        with patch.object(reader, "PAGE_SIZE", 2):
            one = self.read()
            two = self.read(cursor=one["nextCursor"])
        self.assertEqual([e["name"] for e in one["entries"]], ["a", "B"])
        self.assertEqual([e["name"] for e in two["entries"]], ["c", "d"])
        with patch.object(reader, "SCAN_LIMIT", 1):
            limited = self.read()
        self.assertTrue(limited["limited"])
        self.assertEqual(len(limited["entries"]), 1)

    def test_accept_revalidates_directory_without_listing(self):
        with patch.object(reader.os, "scandir", side_effect=AssertionError("no read on validation")):
            result = self.read("~/projects", validate=True)
        self.assertTrue(result["ok"])
        self.assertNotIn("entries", result)
        (self.home / "gone").mkdir()
        (self.home / "gone").rmdir()
        self.assertEqual(self.read("~/gone", validate=True)["code"], "not_found")

    def test_permission_and_bad_path_errors_are_explicit(self):
        with patch.object(reader.os, "scandir", side_effect=PermissionError(errno.EACCES, "denied")):
            self.assertEqual(self.read()["code"], "permission")
        self.assertEqual(self.read("bad\npath")["code"], "invalid_path")
        self.assertEqual(self.read("~somebody")["code"], "invalid_path")
        (self.home / "file").write_text("x")
        self.assertEqual(self.read("~/file")["code"], "not_directory")

    def test_spaces_unicode_and_shell_syntax_are_plain_path_data(self):
        name = "Проект с пробелами '$() ; quotes"
        path = self.home / name
        path.mkdir()
        result = subprocess.run([sys.executable, str(SOURCE), json.dumps({"path": str(path)})], capture_output=True, text=True, check=True)
        self.assertEqual(json.loads(result.stdout)["path"], str(path))
        self.assertEqual(list(path.iterdir()), [])

    def test_trailing_spaces_in_a_directory_are_not_stripped(self):
        path = self.home / "directory "
        path.mkdir()
        self.assertEqual(self.read(str(path))["path"], str(path))
        self.assertIn("directory ", [e["name"] for e in self.read()["entries"]])

    def test_query_filters_before_pagination_and_casefolds_unicode(self):
        for name in ["aaa", "bbb", "ccc", "Проект", "ПРОБА", "Straße"]:
            (self.home / name).mkdir()
        with patch.object(reader, "PAGE_SIZE", 1):
            first = self.read(query="пРо")
            second = self.read(query="пРо", cursor=first["nextCursor"])
        self.assertEqual(first["query"], "пРо")
        self.assertEqual({first["entries"][0]["name"], second["entries"][0]["name"]}, {"Проект", "ПРОБА"})
        self.assertIsNone(second["nextCursor"])
        self.assertEqual(self.read(query="STRASSE")["entries"][0]["name"], "Straße")

    def test_query_matches_prefixes_hidden_and_symlinks(self):
        (self.home / ".my-project").mkdir()
        self.assertEqual(self.read(query=".my-pro")["entries"], [])
        self.assertEqual(self.read(query=".my-pro", hidden=True)["entries"][0]["name"], ".my-project")
        self.assertEqual(self.read(query="PROJE")["entries"][0]["name"], "projects")
        self.assertTrue(self.read(query="PROJE")["entries"][0]["link"])
        self.assertEqual(self.read(query="oject")["entries"], [])
        self.assertEqual(self.read(query="no-matches")["entries"], [])

    def test_query_rejects_bad_types_and_excessive_length(self):
        for query in [None, 12, [], "a" * 1025]:
            self.assertEqual(self.read(query=query)["code"], "invalid_query")
        self.assertTrue(self.read(query="a" * 1024)["ok"])


if __name__ == "__main__":
    unittest.main()
