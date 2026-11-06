#!/usr/bin/env python3
"""Fresh install only. Never overwrite a user's existing layout, mods or keys."""
import argparse
import json
import os
from pathlib import Path
import shutil
import tempfile

ROOT = Path(__file__).resolve().parents[1]
MODS = ("trin-brand", "trin-projects", "trin-tabs", "trin-tab-menu", "trin-agents", "trin-quotas")


def ignored_mod_files(directory, names):
    """Flat runtime modules only; don't copy ignored personal files from a dev tree."""
    ignored = []
    for name in names:
        path = Path(directory) / name
        allowed = name == "mod.json" or path.suffix in {".js", ".py", ".svg", ".png", ".md", ".txt"}
        if not allowed or path.is_dir() or path.is_symlink() or name.startswith(".") or ".local." in name:
            ignored.append(name)
    return ignored


def install(config):
    source = ROOT / "layout/trin"
    targets = [(source / "layouts/trin.json", config / "layouts/trin.json")]
    targets += [(source / "mods" / name, config / "mods" / name) for name in MODS]
    assets = {
        "trin-brand": {"deleeva-mark-dark.png": source / "mods/trin-brand/deleeva-mark-dark.png"},
        "trin-projects": {name + ".svg": source / "mods/trin-projects" / (name + ".svg")
                          for name in ("home", "arrow-up", "eye", "eye-off")},
        "trin-providers": {
            **{name + ".svg": source / "mods/trin-agents" / ("provider-" + name + ".svg")
               for name in ("openai", "anthropic")},
            **{name + ".svg": source / "mods/trin-quotas" / ("provider-" + name + ".svg")
               for name in ("ollama", "kimi")},
        },
    }
    # All conflict and input checks happen before any writes.
    for src, dst in targets:
        if not src.exists():
            raise ValueError("Missing source: " + str(src))
        if dst.exists() or dst.is_symlink():
            raise ValueError("Refusing to overwrite: " + str(dst))
    for name, files in assets.items():
        dst = config / "assets" / name
        if dst.exists() or dst.is_symlink():
            raise ValueError("Refusing to overwrite assets: " + str(dst))
        if any(not path.is_file() for path in files.values()):
            raise ValueError("Missing asset sources")
    for name in ("layouts", "mods", "assets"):
        if (config / name).is_symlink():
            raise ValueError("Refusing symlinked target directory: " + name)
    keyfile = config / "keys.json"
    if keyfile.is_symlink():
        raise ValueError("Refusing symlinked keys.json")
    keys = json.loads(keyfile.read_text()) if keyfile.exists() else {}
    incoming = json.loads((source / "keys.json").read_text())
    if not isinstance(keys, dict) or set(incoming) != {"layout:trin"}:
        raise ValueError("Unexpected keymap format")
    if "layout:trin" in keys:
        raise ValueError("Refusing to overwrite layout:trin key bindings")
    keys.update(incoming)
    # Stage on the target filesystem; existing keys are changed only at the end.
    config.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".trin-install-", dir=config) as temp:
        stage = Path(temp)
        staged = []
        for src, dst in targets:
            rel = dst.relative_to(config)
            out = stage / rel
            out.parent.mkdir(parents=True, exist_ok=True)
            if src.is_dir():
                shutil.copytree(src, out, ignore=ignored_mod_files)
            else:
                shutil.copy2(src, out)
            staged.append((out, dst))
        for name, files in assets.items():
            out = stage / "assets" / name
            out.mkdir(parents=True)
            for filename, src in files.items():
                shutil.copy2(src, out / filename)
            staged.append((out, config / "assets" / name))
        newkeys = stage / "keys.json"
        newkeys.write_text(json.dumps(keys, ensure_ascii=False, indent=2) + "\n")
        for src, dst in staged:
            dst.parent.mkdir(parents=True, exist_ok=True)
            os.rename(src, dst)
        os.replace(newkeys, keyfile)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config-dir", type=Path, required=True)
    args = parser.parse_args()
    try:
        install(args.config_dir.expanduser().absolute())
    except (ValueError, OSError) as error:
        parser.exit(1, str(error) + "\n")
    print("Installed trin; layout not switched. Run herdr-app check for each mod before switching.")


if __name__ == "__main__":
    main()
