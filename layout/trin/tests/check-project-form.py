"""Build an isolated native form-check package; never opens a live overlay."""
import argparse
import json
import pathlib
import subprocess
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument("--temp-root", required=True)
args = parser.parse_args()
tests = pathlib.Path(__file__).resolve().parent
mod = tests.parent / "mods/trin-projects"
with tempfile.TemporaryDirectory(prefix="projects-native-form-check-", dir=args.temp_root) as folder:
    package = pathlib.Path(folder)
    for filename in ["form.js", "data.js", "store.js", "remote-picker.js", "remote-directory.js", "remote-directory.py", "folder-browser.js"]:
        (package / filename).write_text((mod / filename).read_text())
    template = (tests / "native-form/main.js").read_text()
    for setup in ["", " this.machineOpen = true;", " this.machine = 'remote-check';", " this.fieldErrors = {name:'Введите название.',folder:'Выберите директорию.'};"]:
        (package / "main.js").write_text(template.replace("initProjectForm(this, cx);", "initProjectForm(this, cx);" + setup))
        subprocess.run(["herdr-app", "check", str(package)], check=True)
    fixture = {"ok": True, "path": "/home/check/projects", "realPath": "/mnt/data/projects", "home": "/home/check",
               "entries": [{"name": "normal folder", "link": False, "error": None},
                           {"name": "linked", "link": True, "target": "/mnt/elsewhere", "error": None},
                           {"name": "broken", "link": True, "error": "not_found"}], "nextCursor": "more", "limited": False}
    for state in ["ready", "loading", "error", "search", "empty-search", "initial-loading", "navigation-loading", "more-loading"]:
        imports = 'import { FolderBrowser } from "./folder-browser.js";\nimport { openRemoteFolderPicker } from "./remote-picker.js";\n'
        setup = (' this.machine = "native-check"; const data = ' + json.dumps(fixture) + ';'
                 ' this.remoteBrowser = new FolderBrowser(async () => data); openRemoteFolderPicker(this, cx);'
                 ' this.remotePicker.model.snapshot = data; this.remotePicker.resetting = true; this.remotePicker.path.set_value(data.path); this.remotePicker.resetting = false; this.remotePicker.location = data.path;'
                 ' this.remotePicker.model.loading = ' + ("true" if state in ["loading", "initial-loading", "navigation-loading", "more-loading"] else "false") + ';'
                 + (' this.remotePicker.model.error = "Нет доступа к директории";' if state == "error" else "")
                 + (' this.remotePicker.path.set_value(data.path + "/link");' if state == "search" else "")
                 + (' this.remotePicker.model.snapshot = null;' if state == "initial-loading" else "")
                 + (' this.remotePicker.location = "/another-directory";' if state == "navigation-loading" else "")
                 + (' this.remotePicker.path.set_value(data.path + "/missing"); this.remotePicker.model.loading = false; this.remotePicker.model.snapshot.entries = []; this.remotePicker.model.snapshot.nextCursor = null;' if state == "empty-search" else ""))
        (package / "main.js").write_text(imports + template.replace("initProjectForm(this, cx);", "initProjectForm(this, cx);" + setup))
        subprocess.run(["herdr-app", "check", str(package)], check=True)
