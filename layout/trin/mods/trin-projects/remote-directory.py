"""Read-only, non-recursive POSIX directory picker protocol over Herdr exec."""
import errno
import json
import os
import stat
import sys

PAGE_SIZE = 200
SCAN_LIMIT = 20000


def error_code(error):
    return {errno.EACCES: "permission", errno.EPERM: "permission", errno.ENOENT: "not_found",
            errno.ENOTDIR: "not_directory", errno.ELOOP: "symlink_loop"}.get(error.errno, "io")


def logical_path(value, base, home):
    if not isinstance(value, str) or any(c in value for c in "\0\r\n"):
        raise ValueError("invalid_path")
    if not value or value == "~":
        value = home
    elif value.startswith("~/"):
        value = os.path.join(home, value[2:])
    elif value.startswith("~"):
        raise ValueError("invalid_path")
    if not os.path.isabs(value):
        value = os.path.join(base if isinstance(base, str) and base.startswith("/") else home, value)
    # Logical cd -L semantics: Up through a link returns to its visible parent.
    return "/" + os.path.normpath(value).lstrip("/")


def read_directory(request):
    path = ""
    try:
        if os.name != "posix":
            return {"ok": False, "code": "unsupported", "path": path}
        home = os.path.expanduser("~")
        path = logical_path(request.get("path", ""), request.get("base"), home)
        info = os.stat(path)  # Follows directory links, including other mounts.
        if not stat.S_ISDIR(info.st_mode):
            return {"ok": False, "code": "not_directory", "path": path}
        if not os.access(path, os.X_OK):
            return {"ok": False, "code": "permission", "path": path}
        result = {"ok": True, "path": path, "realPath": os.path.realpath(path), "home": home}
        if request.get("validate"):
            return result
        hidden = request.get("hidden") is True
        query = request.get("query", "")
        if not isinstance(query, str) or len(query) > 1024:
            return {"ok": False, "code": "invalid_query", "path": path}
        query_folded = query.casefold()
        entries = []
        limited = False
        with os.scandir(path) as directory:
            for index, entry in enumerate(directory):
                if index >= SCAN_LIMIT:
                    limited = True
                    break
                if not hidden and entry.name.startswith("."):
                    continue
                if not entry.name.casefold().startswith(query_folded):
                    continue
                link = entry.is_symlink()
                failure = None
                try:
                    if not stat.S_ISDIR(entry.stat(follow_symlinks=True).st_mode):
                        continue
                except OSError as error:
                    if not link:
                        continue
                    failure = error_code(error)
                target = None
                if link:
                    try:
                        target = os.readlink(entry.path)
                    except OSError:
                        pass
                if any(c in entry.name for c in "\r\n"):
                    failure = "invalid_path"
                entries.append({"name": entry.name, "link": link, "target": target, "error": failure})
        key = lambda entry: (entry["name"].casefold(), entry["name"])
        entries.sort(key=key)
        cursor = request.get("cursor")
        if cursor is not None:
            if not isinstance(cursor, str):
                raise ValueError("invalid_path")
            entries = [entry for entry in entries if key(entry) > (cursor.casefold(), cursor)]
        page = entries[:PAGE_SIZE]
        return {**result, "query": query, "entries": page, "nextCursor": page[-1]["name"] if len(entries) > PAGE_SIZE else None, "limited": limited}
    except OSError as error:
        return {"ok": False, "code": error_code(error), "path": path}
    except (ValueError, TypeError):
        return {"ok": False, "code": "invalid_path", "path": path}


if __name__ == "__main__":
    try:
        request = json.loads(sys.argv[1])
        result = read_directory(request) if isinstance(request, dict) else {"ok": False, "code": "invalid_path"}
    except (ValueError, IndexError):
        result = {"ok": False, "code": "invalid_path"}
    print(json.dumps(result, ensure_ascii=True, separators=(",", ":")))
