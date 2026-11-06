"""Read-only OpenCode V2 task snapshot. Run locally or over Herdr exec(on=...)."""

import base64
import concurrent.futures
import copy
import json
import hashlib
import math
import os
from pathlib import Path
import re
import sys
import time
import urllib.parse
import urllib.request
import urllib.error


class Unavailable(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class API:
    def __init__(self):
        # This is OpenCode's documented service discovery contract. Credentials
        # stay on the owning machine; neither the registration nor auth is output.
        state = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state")))
        info = json.loads((state / "opencode/service.json").read_text())
        self.url = info["url"].rstrip("/")
        parsed = urllib.parse.urlsplit(self.url)
        if parsed.scheme not in ("http", "https") or parsed.username or parsed.password:
            raise Unavailable()
        self.headers = {"Accept": "application/json"}
        # build_opener installs HTTPSHandler (and loads the CA store) even for
        # HTTP. Construct only the handlers this service actually needs, once.
        # The shared opener is stateless between requests, including pool reads.
        self.opener = urllib.request.OpenerDirector()
        for handler in (urllib.request.UnknownHandler(), urllib.request.HTTPHandler(),
                        urllib.request.HTTPDefaultErrorHandler(), NoRedirect(),
                        urllib.request.HTTPErrorProcessor()):
            self.opener.add_handler(handler)
        if parsed.scheme == "https":
            self.opener.add_handler(urllib.request.HTTPSHandler())
        if "password" in info:
            token = base64.b64encode(("opencode:" + info["password"]).encode()).decode()
            self.headers["Authorization"] = "Basic " + token
        self.deadline = time.monotonic() + 18
        health = self.get("/api/info")
        if health.get("pid") != info["pid"]:
            raise Unavailable()
        self.identity = hashlib.sha256((self.url + str(health.get("pid"))).encode()).hexdigest()

    def get(self, path, **query):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise Unavailable()
        url = self.url + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        request = urllib.request.Request(url, headers=self.headers, method="GET")
        # Never send local service credentials through a proxy or a redirect.
        with self.opener.open(request, timeout=min(2, remaining)) as response:
            raw = response.read(4 * 1024 * 1024 + 1)
            if len(raw) > 4 * 1024 * 1024:
                raise Unavailable()
            return json.loads(raw)


def title_of(title):
    return re.sub(r"^(?:OC|OpenCode)\s*\|\s*", "", title or "").strip()


def program_of(command):
    # Only a program name is returned, never complete arguments/env assignments
    # (they can contain credentials). This is a tooltip, not a command log.
    match = re.match(r"^\s*([A-Za-z0-9_./+-]+)(?:\s|$)", command or "")
    return Path(match[1]).name[:40] if match else "shell"


def token_total(usage):
    if not isinstance(usage, dict):
        return None
    cache = usage.get("cache") or {}
    values = [usage.get("input"), usage.get("output"), usage.get("reasoning"), cache.get("read"), cache.get("write")]
    if any(not isinstance(n, (int, float)) or not math.isfinite(n) or n < 0 for n in values):
        return None
    return sum(values)


def scan_duration(api, session_id, previous=None, running=False, now=None):
    """Incremental sum of assistant-step durations, never session age.

    Only compact accounting state crosses machines, not prompts or responses.
    A bounded historical scan continues on later polls; do not display its
    partial total as the whole session's time.
    """
    now = int(time.time() * 1000) if now is None else now
    state = copy.deepcopy(previous) if previous and previous.get("sessionID") == session_id else {
        "sessionID": session_id, "totalMs": 0, "lastID": "", "cursor": None,
        "pending": {}, "ready": False,
    }
    path = "/api/session/" + urllib.parse.quote(session_id, safe="") + "/message"

    def account(message):
        stamp = message.get("time") or {}
        start, end = stamp.get("created"), stamp.get("completed")
        if not isinstance(start, (int, float)):
            return
        if isinstance(end, (int, float)) and end >= start:
            state["totalMs"] += end - start
            state["pending"].pop(message["id"], None)
        else:
            state["pending"][message["id"]] = start

    # Revisit open steps: the timeline cursor cannot observe an update to an
    # earlier message. Keeping these IDs prevents lost time and double counting.
    for message_id in list(state["pending"])[-4:]:
        account(api.get(path + "/" + urllib.parse.quote(message_id, safe=""))["data"])
    for _ in range(2):
        if getattr(api, "deadline", float("inf")) - time.monotonic() < 3:
            break
        query = {"type": "assistant", "limit": "40"}
        if state["cursor"]:
            query["cursor"] = state["cursor"]
        else:
            query["order"] = "asc"
        page = api.get(path, **query)
        messages = page["data"]
        for message in messages:
            if message["id"] <= state["lastID"]:
                continue
            if message.get("type") == "assistant":
                account(message)
            state["lastID"] = message["id"]
        cursor = (page.get("cursor") or {}).get("next")
        if cursor:
            state["cursor"] = cursor
        state["ready"] = len(messages) < 40
        if state["ready"]:
            break
        if not cursor:
            raise Unavailable()
    # Only the current open step can accrue live time. Interrupted historical
    # steps without a completion timestamp must not turn into day-long timers.
    live_start = state["pending"].get(state["lastID"]) if running and state["ready"] else None
    elapsed = state["totalMs"] + (max(0, now - live_start) if live_start is not None else 0)
    return {
        "durationState": state, "durationReady": state["ready"],
        "durationMs": elapsed if state["ready"] else None,
        "durationAt": now if live_start is not None else 0, "durationRunning": live_start is not None,
    }


def context_message(api, session):
    """Match OpenCode's lastAssistantWithUsage, without downloading all history."""
    path = "/api/session/" + urllib.parse.quote(session["id"], safe="") + "/message"
    boundary = (session.get("revert") or {}).get("messageID")
    before_boundary = not boundary
    cursor = None
    # Usually just the open assistant + user + previous assistant are needed.
    # The API has no metadata-only projection; retain the same 120-message bound
    # for revert/compaction, but do not fetch 40 bulky messages in the common case.
    for _ in range(30):
        query = {"limit": "4"}
        if cursor:
            query["cursor"] = cursor
        else:
            query["order"] = "desc"
        page = api.get(path, **query)
        for message in page["data"]:
            if not before_boundary:
                if message["id"] == boundary:
                    before_boundary = True
                continue
            if message.get("type") == "compaction" and message.get("status") == "completed":
                return None
            if message.get("type") == "assistant" and message.get("tokens") is not None:
                return message
        if len(page["data"]) < 4:
            return None
        cursor = (page.get("cursor") or {}).get("next")
        if not cursor:
            break
    raise Unavailable()


def context_limit(api, session, model, cache):
    # Model limits can differ by location/provider configuration. Only export
    # the numeric limit, never model settings, headers or credentials.
    directory = (session.get("location") or {}).get("directory")
    if directory not in cache:
        try:
            query = {"location[directory]": directory} if directory else {}
            items = api.get("/api/model", **query)["data"]
            cache[directory] = {(m.get("providerID"), m.get("id")): (m.get("limit") or {}).get("context") for m in items}
        except Exception:
            cache[directory] = {}
    value = cache[directory].get((model.get("providerID"), model.get("id")))
    return value if isinstance(value, (int, float)) and math.isfinite(value) and value > 0 else None


def session_metrics(api, session, target, running, model_cache=None):
    model = session.get("model") or {}
    result = {"modelID": model.get("id"), "providerID": model.get("providerID"),
              "variant": model.get("variant"), "totalTokens": token_total(session.get("tokens"))}
    # Validated scalar buckets only: no prompts or message content cross the
    # bridge. Keep cache reads/writes and reasoning separate so sums reconcile.
    if result["totalTokens"] is not None:
        usage = session["tokens"]
        result["usage"] = {"input": usage["input"], "output": usage["output"], "reasoning": usage["reasoning"],
                           "cacheRead": usage["cache"]["read"], "cacheWrite": usage["cache"]["write"]}
    context_model = model
    version = hashlib.sha256(json.dumps([session.get("time"), session.get("tokens"),
        session.get("revert"), model, running], sort_keys=True).encode()).hexdigest()
    cached = target.get("metricsCache") or {}
    reusable = (cached.get("sessionID") == session["id"] and cached.get("version") == version
                and 0 <= time.time() - cached.get("at", 0) < 30
                and session["id"] not in getattr(api, "dirty_sessions", set()))
    try:
        if reusable and "contextTokens" in cached:
            result["contextTokens"] = cached["contextTokens"]
            context_model = cached.get("contextModel") or model
        else:
            message = context_message(api, session)
            result["contextTokens"] = token_total(message["tokens"]) if message else None
            if message:
                context_model = message.get("model") or model
    except Exception:
        result["contextError"] = "Current context usage is temporarily unavailable"
    result["contextModelID"] = context_model.get("id")
    result["contextProviderID"] = context_model.get("providerID")
    result["contextLimit"] = context_limit(api, session, context_model, model_cache if model_cache is not None else {})
    previous = target.get("durationState")
    try:
        if reusable and not running and previous and previous.get("ready") and not previous.get("pending"):
            result.update(durationState=previous, durationReady=True, durationMs=previous["totalMs"],
                          durationAt=0, durationRunning=False)
        else:
            result.update(scan_duration(api, session["id"], previous, running))
    except Exception:
        # Keep task polling independent of history/metadata availability.
        result["durationError"] = "Session timing history is temporarily unavailable"
        if previous and previous.get("sessionID") == session["id"]:
            result["durationState"] = previous
    if "contextError" not in result and "durationError" not in result:
        result["metricsCache"] = {"sessionID": session["id"], "version": version,
            "at": cached["at"] if reusable else time.time(),
            "contextTokens": result.get("contextTokens"),
            "contextModel": {key: context_model.get(key) for key in ("id", "providerID")}}
    return result


def match_session(api, target):
    title = title_of(target.get("title"))
    if not title:
        return None
    # Some CLI terminal titles are already ellipsized before Herdr receives
    # them. Only an explicit long ellipsis suffix permits prefix matching;
    # ordinary titles still require equality. Never choose the newest match.
    prefix = title[:-1].rstrip() if title.endswith("…") else ""
    truncated = len(prefix) >= 16

    def matches(item):
        actual = item.get("title", "")
        return actual == title or (truncated and actual.startswith(prefix))

    # OSC record IDs, when present, are stronger evidence than a title search.
    for hint in target.get("hints", []):
        try:
            item = api.get("/api/session/" + urllib.parse.quote(hint, safe=""))["data"]
        except Exception:
            continue
        if matches(item):
            return item
    found = []
    cursor = None
    for _ in range(5):
        query = {"search": prefix if truncated else title, "limit": "100"}
        if cursor:
            query["cursor"] = cursor
        page = api.get("/api/session", **query)
        items = page["data"]
        found.extend(item for item in items if matches(item))
        # V2 cursors may include a next cursor even at the final item.
        if len(items) < 100:
            break
        cursor = page.get("cursor", {}).get("next")
        if not cursor:
            break
    else:
        raise Unavailable()
    unique = {item["id"]: item for item in found}
    exact = {sid: item for sid, item in unique.items() if item.get("title") == title}
    if exact:
        unique = exact
    if len(unique) == 1:
        return next(iter(unique.values()))
    # Launch cwd can differ from the session's location. Use it only to
    # disambiguate equal titles, never pick the first/most recently used session.
    cwd = target.get("cwd")
    same_place = [s for s in unique.values() if s.get("location", {}).get("directory") == cwd]
    return same_place[0] if len(same_place) == 1 else None


def collect(api, targets):
    cache = {}
    matched = {}
    output = {}
    model_cache = {}
    for target in targets:
        try:
            session = match_session(api, target)
        except Exception:
            output[target["address"]] = {"title": target["title"], "error": "Session lookup is temporarily unavailable"}
            continue
        if session is None:
            output[target["address"]] = {"title": target["title"], "error": "Session match is missing or ambiguous"}
            continue
        cache[session["id"]] = session
        matched[target["address"]] = session["id"]
    if not matched:
        return output

    active = api.get("/api/session/active")["data"]
    if len(active) > 128:
        raise Unavailable()

    def shells_at(directory):
        return api.get("/api/shell", **{"location[directory]": directory})["data"]

    shell_cache = getattr(api, "shell_cache", None)
    if shell_cache and 0 <= time.time() - shell_cache.get("at", 0) < 18:
        shells = {s["id"]: s for s in shell_cache["items"]}
    else:
        locations = api.get("/api/debug/location")
        directories = set(item["directory"] for item in locations)
        if len(directories) > 64:
            raise Unavailable()
        shells = {}
        with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
            for items in pool.map(shells_at, sorted(directories)):
                for shell in items:
                    if shell.get("status") == "running" and shell.get("metadata", {}).get("sessionID"):
                        # Only an allowlisted snapshot may survive exec. Never
                        # cache command arguments/env or arbitrary shell metadata.
                        shells[shell["id"]] = {"id": shell["id"], "program": program_of(shell.get("command")),
                            "pid": shell.get("pid"), "metadata": {"sessionID": shell["metadata"]["sessionID"]}}
        api.shell_cache = {"at": time.time(), "items": list(shells.values())}

    def ancestors(session_id):
        chain = set()
        while session_id:
            if session_id in chain or len(chain) >= 32:
                raise Unavailable()
            chain.add(session_id)
            if session_id not in cache:
                cache[session_id] = api.get("/api/session/" + urllib.parse.quote(session_id, safe=""))["data"]
            session_id = cache[session_id].get("parentID")
        return chain

    # Internal OpenCode subagents are sessions, not Herdr child terminals.
    running = {sid: ancestors(sid) for sid, state in active.items() if state.get("type") == "running"}
    owners = {shell["id"]: ancestors(shell["metadata"]["sessionID"]) for shell in shells.values()}
    for target in targets:
        root = matched.get(target["address"])
        if root is None:
            continue
        owned_shells = [shell for shell in shells.values() if root in owners[shell["id"]]]
        own = sum(shell["metadata"]["sessionID"] == root for shell in owned_shells)
        children = sorted(sid for sid, chain in running.items() if sid != root and root in chain)
        output[target["address"]] = {
            "title": target["title"],
            "sessionID": root,
            "subagents": len(children),
            "subagentsDirect": sum(cache[sid].get("parentID") == root for sid in children),
            "subagentsNested": sum(cache[sid].get("parentID") != root for sid in children),
            "subagentDetails": [
                {"title": cache[sid].get("title", "Untitled session"),
                 "modelID": (cache[sid].get("model") or {}).get("id"),
                 "variant": (cache[sid].get("model") or {}).get("variant"),
                 "totalTokens": token_total(cache[sid].get("tokens")),
                 "nested": cache[sid].get("parentID") != root}
                for sid in children[:8]
            ],
            "shells": len(owned_shells),
            "shellsOwn": own,
            "shellsInherited": len(owned_shells) - own,
            "shellDetails": [
                {
                    "program": shell["program"],
                    "ownerTitle": cache[shell["metadata"]["sessionID"]].get("title", "Untitled session"),
                    "inherited": shell["metadata"]["sessionID"] != root,
                    "ownerActive": shell["metadata"]["sessionID"] in running,
                    "pid": shell.get("pid"),
                }
                for shell in owned_shells[:8]
            ],
            "meta": session_metrics(api, cache[root], target, root in running, model_cache),
        }
    return output


def main():
    targets = json.loads(sys.argv[1])
    try:
        result = collect(API(), targets)
    except Exception as error:
        # Errors must never leak the registration, credentials or command output.
        reason = f"HTTP {error.code}" if isinstance(error, urllib.error.HTTPError) else type(error).__name__
        result = {t["address"]: {"title": t["title"], "error": f"OpenCode task API is unavailable ({reason})"} for t in targets}
    print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    main()
