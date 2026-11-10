import importlib.util
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("installer", Path(__file__).with_name("install-layout.py"))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

scan_spec = importlib.util.spec_from_file_location("privacy", Path(__file__).with_name("check-public.py"))
privacy = importlib.util.module_from_spec(scan_spec)
scan_spec.loader.exec_module(privacy)


class PortabilityTest(unittest.TestCase):
    def scan(self, content):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            (root / "example.js").write_text(content)
            original = privacy.ROOT
            output = io.StringIO()
            try:
                privacy.ROOT = root
                with contextlib.redirect_stdout(output):
                    result = privacy.main()
            finally:
                privacy.ROOT = original
            return result, output.getvalue()

    def test_private_tooling_is_rejected_without_printing_the_command(self):
        for tool in ["agent-" + "platform", "agent-" + "tooling", "or" + "ca", "w" + "t"]:
            with self.subTest(tool=tool):
                result, output = self.scan('exec("' + tool + '", []);')
                self.assertTrue(result)
                self.assertIn("nonportable-cli", output)
                self.assertNotIn('exec("' + tool, output)

    def test_documented_standard_tools_and_example_paths_are_allowed(self):
        result, _ = self.scan('herdr-app hc curl python3 git node go docker-compose ssh /home/user/project /Users/me/project /home/remote/project /home/check/project https://quota.example.com/v1/quotas')
        self.assertFalse(result)

    def test_unexpected_personal_home_path_is_rejected(self):
        result, output = self.scan('/Users/' + 'someone-private/project/config')
        self.assertTrue(result)
        self.assertIn("personal-infrastructure", output)


class InstallTest(unittest.TestCase):
    def test_private_mod_files_are_never_installed_from_a_dev_checkout(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "private").mkdir()
            names = ["main.js", "mod.json", "auth.json", ".env", "config.local.js", "gpui-kit.d.ts", "private"]
            excluded = installer.ignored_mod_files(root, names)
            self.assertEqual(set(excluded), set(names) - {"main.js", "mod.json"})

    def test_fresh_install_merges_keys_and_copies_real_assets(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "keys.json").write_text('{"global":{"ctrl-x":"example"}}')
            installer.install(root)
            keys = json.loads((root / "keys.json").read_text())
            self.assertEqual(keys["global"], {"ctrl-x": "example"})
            self.assertIn("layout:trin", keys)
            self.assertTrue((root / "assets/trin-providers/openai.svg").is_file())
            self.assertEqual((root / "assets/trin-providers/opencode-logo.svg").read_bytes(),
                             (installer.ROOT / "layout/trin/mods/trin-quotas/provider-opencode.svg").read_bytes())
            self.assertFalse((root / "assets/trin-providers/openai.svg").is_symlink())
            self.assertFalse((root / "mods/trin-quotas/gpui-kit.d.ts").exists())
            with self.assertRaises(ValueError):
                installer.install(root)

    def test_conflict_does_not_touch_existing_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "keys.json").write_text('{"layout:trin":{}}')
            before = (root / "keys.json").read_bytes()
            with self.assertRaises(ValueError):
                installer.install(root)
            self.assertEqual(before, (root / "keys.json").read_bytes())
            self.assertFalse((root / "mods").exists())

    def test_dangling_symlink_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "layouts").mkdir()
            (root / "layouts/trin.json").symlink_to(root / "missing")
            with self.assertRaises(ValueError):
                installer.install(root)
            self.assertTrue((root / "layouts/trin.json").is_symlink())
