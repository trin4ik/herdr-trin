"""Bounded OpenCode SSE -> Herdr exec bridge, local or exec(on=machine).

No daemon, ports, registration files, or credentials leave the owning machine.
The caller injects activity.py as trin_activity so remote installation is not
needed. Subscribe BEFORE reconciliation to close the snapshot/subscribe race.
Herdr buffers exec output and limits calls to 30s; yield on a meaningful event
or after 18s, then reconnect with a fresh snapshot. Whole call budget is 24s.
"""

from contextlib import contextmanager
import copy
import hashlib
import http.client
import json
import socket
import select
import sys
import time
import urllib.parse

from trin_activity import API, Unavailable, collect


EVENTS = {
    "session.created", "session.deleted", "session.forked", "session.renamed",
    "session.model.selected", "session.agent.selected", "session.metadata.updated",
    "session.moved", "session.status", "session.idle",
    "session.execution.started", "session.execution.succeeded",
    "session.execution.failed", "session.execution.interrupted",
    "session.step.started", "session.step.ended", "session.step.failed",
    "session.usage.updated", "session.compaction.started", "session.compaction.ended", "session.compaction.failed",
    "session.revert.staged", "session.revert.cleared", "session.revert.committed",
    "session.shell.started", "session.shell.ended",
    "shell.created", "shell.exited", "shell.deleted", "location.shutdown",
    "model.updated", "provider.updated", "config.updated", "models-dev.refreshed",
}


class SSE:
    """Incremental SSE framing, including fragmented UTF-8 and multiline data."""
    def __init__(self):
        self.buffer = b""
        self.data = []
        self.size = 0

    def feed(self, chunk):
        self.buffer += chunk
        if len(self.buffer) + self.size > 1024 * 1024:
            raise Unavailable()
        events = []
        while b"\n" in self.buffer:
            line, self.buffer = self.buffer.split(b"\n", 1)
            line = line.rstrip(b"\r")
            if not line:
                if self.data:
                    try:
                        item = json.loads(b"\n".join(self.data))
                        if isinstance(item, str):
                            item = json.loads(item)
                        if isinstance(item, dict):
                            events.append(item)
                    except (ValueError, UnicodeError):
                        raise Unavailable() from None
                self.data, self.size = [], 0
            elif line.startswith(b"data:"):
                value = line[5:].removeprefix(b" ")
                self.data.append(value)
                self.size += len(value)
        return events


def fingerprint(snapshot):
    """Clock advancement alone is NOT a data change (avoids a busy loop)."""
    stable = copy.deepcopy(snapshot)
    for info in stable.values():
        meta = info.get("meta") or {}
        meta.pop("metricsCache", None)
        meta.pop("durationAt", None)
        if meta.get("durationRunning"):
            meta.pop("durationMs", None)
    return hashlib.sha256(json.dumps(stable, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


@contextmanager
def subscribe(api):
    url = urllib.parse.urlsplit(api.url)
    cls = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    connection = cls(url.hostname, url.port, timeout=2)
    try:
        path = url.path.rstrip("/") + "/api/event"
        connection.request("GET", path, headers={**api.headers, "Accept": "text/event-stream"})
        sock = connection.sock
        response = connection.getresponse()
        if response.status != 200 or "text/event-stream" not in response.getheader("Content-Type", ""):
            raise Unavailable()
        yield response, sock
    finally:
        connection.close()


def packet(snapshot, key, connected, reason, now=None):
    """Send a duration, not a remote wall clock, to avoid machine clock skew."""
    now = int(time.time() * 1000) if now is None else now
    snapshot = copy.deepcopy(snapshot)
    for info in snapshot.values():
        info["eventBridge"] = connected
        meta = info.get("meta") or {}
        if meta.get("durationRunning") and meta.get("durationMs") is not None:
            meta["durationMs"] += max(0, now - meta["durationAt"])
            meta["durationAt"] = 0  # replaced with the receiving client's clock
    return {"snapshot": snapshot, "key": key, "connected": connected, "reason": reason}


def invalidations(events):
    # Never retain text deltas, prompts, or arbitrary event bodies in a batch.
    result = []
    for event in events:
        if event.get("type") not in EVENTS and not event.get("type", "").startswith(("shell.", "session.shell.", "location.")):
            continue
        data = event.get("data") or event.get("properties") or {}
        sid = data.get("sessionID") if isinstance(data, dict) else None
        result.append({"type": event["type"], "data": {"sessionID": sid}})
    return result


def follow(api, targets, previous=None, budget=24):
    targets = copy.deepcopy(targets)
    started = time.monotonic()
    deadline, listen_end = started + budget, started + min(18, budget - 4)
    api.deadline = min(deadline, started + 12)
    with subscribe(api) as (response, sock):
        snapshot = collect(api, targets)
        key = fingerprint(snapshot)
        # Initial state, reconciliation across a reconnect gap, and bounded
        # history bootstrap are delivered without waiting for another event.
        # A not-yet-identifiable session is NOT a disconnected event stream.
        # Keep listening for its creation/model/title updates; fingerprinting
        # prevents unchanged lookup errors from causing a reconnect busy loop.
        if key != previous:
            return packet(snapshot, key, True, "reconcile")
        parser = SSE()
        while time.monotonic() < listen_end:
            sock.settimeout(max(0.01, listen_end - time.monotonic()))
            try:
                chunk = response.read1(65536)
            except (TimeoutError, socket.timeout):
                return packet(snapshot, key, True, "renew")
            if not chunk:
                raise Unavailable()
            current_events = invalidations(parser.feed(chunk))
            if not current_events:
                continue
            # Allow the corresponding read-model projection to commit, and
            # coalesce events emitted by the same step. Text deltas never query.
            # The buffered exec cannot publish a stream. Batch for two seconds:
            # at most ~one rebuild/sec including the next reconnect snapshot.
            # Drain the accumulated events BEFORE collecting, not one rebuild
            # per socket chunk. read1 returns buffered data without waiting.
            events = []
            batch_end = min(time.monotonic() + 2, listen_end)
            # The first chunk is also part of this batch (including shell events).
            # See below: the parsed events are retained, not raw event payloads.
            events.extend(current_events)
            while time.monotonic() < batch_end:
                # A socket timeout poisons HTTPResponse's buffered reader. Wait
                # for readiness instead so an unchanged batch can keep listening.
                if not select.select([sock], [], [], max(0, batch_end - time.monotonic()))[0]:
                    break
                sock.settimeout(max(0.01, listen_end - time.monotonic()))
                chunk = response.read1(65536)
                if not chunk:
                    raise Unavailable()
                events.extend(invalidations(parser.feed(chunk)))
                if len(events) > 4096:
                    # Only invalidation categories/IDs are needed; bound memory.
                    events = [{"type": "location.shutdown"}, {"type": "session.usage.updated"}]
            for target in targets:
                meta = (snapshot.get(target["address"], {}).get("meta") or {})
                for name in ("durationState", "metricsCache"):
                    if name in meta:
                        target[name] = meta[name]
            if any(e.get("type", "").startswith(("shell.", "session.shell.", "location."))
                   or e.get("type") in ("session.deleted", "session.moved") for e in events):
                api.shell_cache = None
            # A usage/step/compaction update can precede session.time.updated.
            # Missing event IDs conservatively invalidate every metrics cache.
            api.dirty_sessions = set()
            for event in events:
                kind = event.get("type", "")
                if kind.startswith(("session.usage.", "session.step.", "session.compaction.", "session.revert.")):
                    data = event.get("data") or event.get("properties") or {}
                    sid = data.get("sessionID") if isinstance(data, dict) else None
                    if sid:
                        api.dirty_sessions.add(sid)
                    else:
                        for target in targets:
                            target.pop("metricsCache", None)
            api.deadline = deadline
            snapshot = collect(api, targets)
            next_key = fingerprint(snapshot)
            api.dirty_sessions = set()
            if next_key != key:
                return packet(snapshot, next_key, True, "event")
        return packet(snapshot, key, True, "renew")


def main():
    targets = json.loads(sys.argv[1])
    previous = json.loads(sys.argv[2]) if len(sys.argv) > 2 else None
    state = json.loads(sys.argv[3]) if len(sys.argv) > 3 else {}
    api = None
    try:
        api = API()
        # Service restart/address changes cannot reuse another hub's shell IDs.
        identity = api.identity
        if state.get("identity") == identity:
            api.shell_cache = state.get("shells")
        result = follow(api, targets, previous)
        shells = getattr(api, "shell_cache", None)
        result["state"] = {"identity": identity,
                           "shells": shells if shells and len(shells["items"]) <= 512 else None}
    except Exception:
        # A failed stream must not erase cached counts or disclose credentials,
        # prompts, URLs or raw event payloads. Retry is backed off by the caller.
        try:
            if api is None:
                raise Unavailable()
            api.deadline = time.monotonic() + 3
            snapshot = collect(api, targets)
        except Exception:
            snapshot = {t["address"]: {"title": t["title"], "error": "OpenCode event bridge is unavailable"} for t in targets}
        result = packet(snapshot, None, False, "unavailable")
    print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    main()
