"""Opt-in integration smoke: only creates/closes its own disposable shells.

Run with --temp-root <approved temporary directory>. Never runs an agent.
Working sessions and application state files are not edited directly.
"""
import argparse
import json
import pathlib
import shlex
import subprocess
import tempfile
import time
import urllib.parse


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--temp-root", required=True)
    parser.add_argument("--hide-sidebar", action="store_true", help="Also verify commands with sidebar hidden; restores it afterwards")
    args = parser.parse_args()
    base = pathlib.Path(args.temp_root)
    root = pathlib.Path(tempfile.mkdtemp(prefix="projects-smoke-", dir=base))
    (root / "child").mkdir()
    shared = pathlib.Path.home() / ".config/herdr-app/shared.json"

    def state():
        return json.loads(shared.read_text())["trin.projects.v1"]

    def cmd(name, params=None):
        argv = ["herdr-app", "do", name] + ([json.dumps(params)] if params is not None else [])
        result = subprocess.run(argv, text=True, capture_output=True, timeout=35)
        value = json.loads(result.stdout or "{}")
        if result.returncode or value.get("error"):
            raise RuntimeError((argv, result.stdout, result.stderr))
        return value

    def wait(check, label):
        # Bounded diagnostic assertions, not runtime polling in the mod.
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            value = check()
            if value:
                return value
            time.sleep(0.08)
        raise RuntimeError("Timed out: " + label)

    def spaces():
        return json.loads(subprocess.check_output(["herdr-app", "ls"], text=True))["spaces"]

    def get(terminal):
        return json.loads(subprocess.check_output(["hc", "get", terminal, "--json"], text=True))

    encode = lambda value: urllib.parse.quote(value, safe="")
    previous = state()["selected"]
    project = None
    owned = []
    report = {"root": str(root)}
    before = {t["terminal"] for s in spaces() for tab in s["tabs"] for t in tab["terminals"]}
    try:
        cmd("overlay.close")
        initial = cmd("space.new", {"name": "Projects lifecycle smoke", "folder": str(root), "machine": ""})
        owned.append(initial["space"])
        cmd("trin-projects.reconcile")
        project = wait(lambda: next((p for p in state()["projects"] if p["space"] == initial["space"]), None), "adoption")
        pid = project["id"]
        cmd("trin-projects.select--" + encode(pid))
        wait(lambda: state()["selected"] == pid, "selection")
        marker = root / "first-cwd"
        subprocess.check_call(["hc", "send", initial["terminal"], "cd " + shlex.quote(str(root / "child")) + "; pwd -P > " + shlex.quote(str(marker))], stdout=subprocess.DEVNULL)
        # Core's cwd can remain the launch cwd for a shell without OSC 7.
        # Verify the real shell's pwd, not merely that the tab was created.
        wait(lambda: marker.exists() and marker.read_text().strip() == str((root / "child").resolve()), "first terminal cd")
        if args.hide_sidebar:
            cmd("layout.hide", {"id": "sidebar"})
        cmd("trin-projects.new-tab--" + encode(pid))
        native = wait(lambda: next((s for s in spaces() if s["id"] == initial["space"] and len(s["tabs"]) == 2), None), "second tab")
        terminal = next(t for t in native["tabs"] if t["id"] != initial["tab"])["terminals"][0]["terminal"]
        actual = get(terminal)
        assert actual["state"] == "live" and actual["cwd"] == str(root), actual
        report["secondTabRootVerified"] = True
        if args.hide_sidebar:
            report["commandsWorkWithSidebarHidden"] = True
            cmd("layout.show", {"id": "sidebar"})
        for tab in native["tabs"]:
            cmd("tab.close", {"id": tab["id"], "confirmed": True})
        wait(lambda: all(s["id"] != initial["space"] for s in spaces()), "native space closed")
        cmd("trin-projects.reconcile")
        assert next(p for p in state()["projects"] if p["id"] == pid)["folder"] == str(root)
        report["projectSurvivesZeroSessions"] = True
        cmd("trin-projects.select--" + encode(pid))
        subprocess.check_call(["herdr-app", "shot", str(base / "projects-empty-smoke.png")], stdout=subprocess.DEVNULL)
        cmd("trin-projects.close-tab")  # Must not close a terminal in another project.
        cmd("trin-projects.new-tab--" + encode(pid))
        reopened = wait(lambda: next((p for p in state()["projects"] if p["id"] == pid and p["space"] != initial["space"]), None), "reopened project")
        owned.append(reopened["space"])
        native = wait(lambda: next((s for s in spaces() if s["id"] == reopened["space"]), None), "new native space")
        actual = get(native["tabs"][0]["terminals"][0]["terminal"])
        assert actual["state"] == "live" and actual["cwd"] == str(root), actual
        assert reopened["name"] == project["name"] and reopened["color"] == project["color"]
        report["stableIDNameRootColorAfterReopen"] = True
    finally:
        if args.hide_sidebar:
            cmd("layout.show", {"id": "sidebar"})
        # Close only IDs acknowledged for this smoke, even after an assertion fails.
        if project:
            current = next((p for p in state()["projects"] if p["id"] == project["id"]), None)
            if current and current["space"] not in owned:
                owned.append(current["space"])
        for sid in owned:
            if any(s["id"] == sid for s in spaces()):
                cmd("space.close", {"id": sid, "confirmed": True})
        if project:
            cmd("trin-projects.remove--" + encode(project["id"]))
            wait(lambda: all(p["id"] != project["id"] for p in state()["projects"]), "remove test project")
        cmd("trin-projects.select--" + encode(previous))
        wait(lambda: state()["selected"] == previous, "restore project")
        after = {t["terminal"] for s in spaces() for tab in s["tabs"] for t in tab["terminals"]}
        assert before <= after, (before, after)
        report["existingTerminalsPreserved"] = True
        report["cleanup"] = True
        (base / "projects-live-smoke.json").write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
