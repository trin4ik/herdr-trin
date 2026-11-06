#!/usr/bin/env python3
"""Heuristic privacy gate over the actual Git candidate set; never prints values."""
from pathlib import Path
import re
import fnmatch
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def candidates():
    result = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                            cwd=ROOT, check=True, capture_output=True)
    return sorted(set(name for name in result.stdout.decode().split("\0") if name))


def main():
    files = candidates()
    patterns = {
        "private-key": re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
        "provider-token": re.compile(r"\b(?:sk-(?:ant-[A-Za-z0-9_-]*|proj-[A-Za-z0-9_-]*|[A-Za-z0-9]{20,})|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[A-Z0-9]{16})\b"),
        "jwt": re.compile(r"\beyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{15,}\b"),
        # Generic rules, without publishing the author's hostnames/usernames.
        "personal-infrastructure": re.compile(r"/(?:Users|home)/(?!(?:user|example|me|remote|check)/)[A-Za-z0-9_.-]+/|/mnt/data/home/(?!(?:user|example|me|remote|check)/)[A-Za-z0-9_.-]+/|https?://(?!(?:opencode\.)?example\.box\b)[A-Za-z0-9.-]+\.box\b", re.I),
        "nonportable-cli": re.compile(r"\b(?:agent[-_ ]platform|agent[-_ ]tooling|orca(?:-cli)?|wt)\b", re.I),
    }
    issues = []
    for name in files:
        path = ROOT / name
        if name in {"AGENTS.md", "CHANGELOG.md"} or any(fnmatch.fnmatch(name, pattern) for pattern in
                [".local/*", "prompts/*", "layout/trin/design/*", "*.local.*", "*/.env", ".env", "*/data/*", "*/credentials/*"]):
            issues.append((name, 0, "private-file-in-git-candidates"))
        if path.is_symlink():
            issues.append((name, 0, "symlink-not-public"))
            continue
        if not path.is_file():
            issues.append((name, 0, "missing-tracked-file"))
            continue
        if path.suffix.lower() in {".pem", ".key", ".sqlite", ".db", ".log"} or path.name in {".env", "auth.json", "service.json"}:
            issues.append((name, 0, "private-file-name"))
        raw = path.read_bytes()
        if b"\0" in raw:
            # Only one deliberately retained brand raster; no terminal screenshots.
            if name != "layout/trin/mods/trin-brand/deleeva-mark-dark.png":
                issues.append((name, 0, "unreviewed-binary"))
            continue
        for line, text in enumerate(raw.decode("utf-8", errors="replace").splitlines(), 1):
            for label, pattern in patterns.items():
                # The scanner necessarily contains the blocked marker signatures.
                if name == "scripts/check-public.py" and label in {"personal-infrastructure", "nonportable-cli"}:
                    continue
                if pattern.search(text):
                    issues.append((name, line, label))
    for name, line, reason in issues:
        print(f"{name}:{line}: {reason} (value redacted)")
    print(f"Checked {len(files)} Git candidates; findings={len(issues)}. Independent secret scan still required.")
    return bool(issues)


if __name__ == "__main__":
    sys.exit(main())
