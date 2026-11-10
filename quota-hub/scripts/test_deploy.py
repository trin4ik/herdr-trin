"""Offline deployment tests: all external service and network commands are fakes."""
import fcntl
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("deploy.sh")
FAKE = r'''#!/usr/bin/env python3
import fcntl, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
state = pathlib.Path(os.environ["TEST_STATE"])
with (state / "calls").open("a") as f:
    f.write(name + " " + " ".join(args) + "\n")
scenario = os.environ.get("SCENARIO", "success")
if name == "uname":
    print("Linux")
elif name == "readlink":
    print(os.path.realpath(args[-1]))
elif name == "git":
    if "--show-toplevel" in args:
        print(os.environ["TEST_ROOT"] if scenario != "wrong-root" else state)
    elif "--show-prefix" in args:
        print("quota-hub/")
    elif "pull" in args and scenario == "pull-fail":
        sys.exit(1)
elif name == "flock":
    try:
        fcntl.flock(int(args[-1]), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        sys.exit(1)
elif name == "go":
    if scenario == "build-fail": sys.exit(1)
    output = pathlib.Path(args[args.index("-o") + 1])
    output.write_text("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_STATE/validation\"\n" + ("exit 1\n" if scenario == "config-fail" else "exit 0\n"))
    output.chmod(0o755)
elif name == "systemctl":
    if "show" in args:
        print("not-found" if scenario == "unit-missing" else "loaded")
    elif "restart" in args:
        p = state / "restarts"
        count = int(p.read_text()) + 1 if p.exists() else 1
        p.write_text(str(count))
        if scenario in ("restart-fail", "rollback-fail") and (count == 1 or scenario == "rollback-fail"):
            sys.exit(1)
    elif "is-active" in args and scenario == "inactive":
        sys.exit(1)
elif name == "curl":
    count = int((state / "restarts").read_text())
    print("302" if scenario in ("redirect", "no-previous") and count == 1 else "200", end="")
elif name == "sleep":
    pass
'''


class DeployTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        self.root = (base / "repo").resolve()
        self.package = self.root / "quota-hub"
        scripts = self.package / "scripts"
        scripts.mkdir(parents=True)
        shutil.copy2(SCRIPT, scripts / "deploy.sh")
        (self.package / "go.mod").write_text("module quota-hub\n")
        self.bin = self.package / "bin"
        self.bin.mkdir()
        self.old = self.bin / "quota-hub"
        self.old.write_text("#!/bin/sh\n# original\nexit 0\n")
        self.old.chmod(0o755)
        self.config = base / "config.json"
        self.config.write_text("{}")
        self.state = base / "state"
        self.state.mkdir()
        tools = base / "tools"
        tools.mkdir()
        for name in ("uname", "readlink", "git", "flock", "go", "systemctl", "curl", "sleep"):
            path = tools / name
            path.write_text(FAKE)
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"],
                        TEST_ROOT=str(self.root), TEST_STATE=str(self.state),
                        CONFIG=str(self.config), HEALTH_URL="http://127.0.0.1:1234/readyz")
        self.env.pop("QUOTA_HUB_DEPLOY_REEXEC", None)
        self.env.pop("QUOTA_HUB_DEPLOY_SCRIPT", None)
        self.script = scripts / "deploy.sh"

    def run_deploy(self, scenario="success", script=None, **env):
        return subprocess.run(["bash", str(script or self.script)],
                              env=dict(self.env, SCENARIO=scenario, **env),
                              text=True, capture_output=True, timeout=20)

    def calls(self):
        return (self.state / "calls").read_text()

    def test_success_and_legacy_symlink(self):
        old_path = self.root.parent / "old-checkout"
        old_path.symlink_to(self.package, target_is_directory=True)
        result = self.run_deploy(script=old_path / "scripts/deploy.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("git -C " + str(self.root) + " pull --ff-only", self.calls())
        self.assertIn("systemctl --user is-active --quiet", self.calls())
        self.assertNotIn("original", self.old.read_text())
        self.assertEqual((self.state / "validation").read_text().splitlines(),
                         ["-config", str(self.config), "-check-config"])

    def test_skip_pull(self):
        result = self.run_deploy(SKIP_PULL="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("pull --ff-only", self.calls())

    def test_preflight_and_validation_leave_binary_unchanged(self):
        for scenario in ("wrong-root", "unit-missing", "pull-fail", "build-fail", "config-fail"):
            with self.subTest(scenario=scenario):
                result = self.run_deploy(scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("original", self.old.read_text())
        self.assertNotIn("systemctl --user restart", self.calls())

    def test_restart_failure_restores_and_verifies_previous(self):
        result = self.run_deploy("restart-fail")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("original", self.old.read_text())
        self.assertEqual((self.state / "restarts").read_text(), "2")
        self.assertIn("сервис готов", result.stdout)

    def test_redirect_causes_rollback(self):
        result = self.run_deploy("redirect")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("original", self.old.read_text())
        self.assertEqual((self.state / "restarts").read_text(), "2")

    def test_inactive_unit_does_not_pass_even_with_http_200(self):
        result = self.run_deploy("inactive")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("не прошёл проверку", result.stdout)
        self.assertIn("original", self.old.read_text())

    def test_failed_rollback_is_reported_and_backup_retained(self):
        result = self.run_deploy("rollback-fail")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("не прошёл проверку", result.stdout)
        backups = list(self.bin.glob(".deploy.*/previous"))
        self.assertEqual(len(backups), 1)
        self.assertIn("original", backups[0].read_text())

    def test_missing_previous_does_not_use_stale_backup(self):
        self.old.unlink()
        (self.bin / "quota-hub.prev").write_text("stale")
        result = self.run_deploy("no-previous")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("откат невозможен", result.stdout)
        self.assertIn("systemctl --user stop", self.calls())
        self.assertNotEqual(self.old.read_text(), "stale")

    def test_lock_blocks_second_deploy(self):
        with (self.bin / ".deploy.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_deploy()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("другой deploy", result.stderr)
        self.assertNotIn("git -C " + str(self.root) + " pull", self.calls())

    def test_requires_config_and_readyz(self):
        for override in ({"CONFIG": ""}, {"HEALTH_URL": "http://127.0.0.1/healthz"}):
            with self.subTest(override=override):
                result = self.run_deploy(**override)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("original", self.old.read_text())


if __name__ == "__main__":
    unittest.main()
