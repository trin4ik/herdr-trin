"""Offline SSE bridge tests. No service, credentials, subprocess or DB touched."""

from contextlib import contextmanager
import copy
import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = Path(__file__).parents[1] / "mods/trin-agents"
spec = importlib.util.spec_from_file_location("trin_activity", ROOT / "activity.py")
activity = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = activity
spec.loader.exec_module(activity)
spec = importlib.util.spec_from_file_location("bridge", ROOT / "bridge.py")
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


def snapshot(**changes):
    return {"t1": {"title": "Task", "sessionID": "ses_1", "shells": 0, "subagents": 0,
                   "meta": {"durationRunning": True, "durationReady": True, "durationMs": 1000,
                            "durationAt": 1000, "durationState": {"totalMs": 0}}, **changes}}


class ParserTests(unittest.TestCase):
    def test_fragmented_utf8_and_double_encoded_event(self):
        parser = bridge.SSE()
        event = {"type": "session.renamed", "data": {"title": "Сессия"}}
        raw = ("data: " + json.dumps(json.dumps(event, ensure_ascii=False), ensure_ascii=False) + "\n\n").encode()
        result = []
        for byte in raw:
            result.extend(parser.feed(bytes([byte])))
        self.assertEqual(result, [event])

    def test_comments_crlf_and_multiline(self):
        parser = bridge.SSE()
        self.assertEqual(parser.feed(b': heartbeat\r\nid: 1\r\ndata: {"type":\r\ndata: "session.status"}\r\n\r\n'),
                         [{"type": "session.status"}])

    def test_invalid_or_oversize_events_fail_closed(self):
        with self.assertRaises(activity.Unavailable):
            bridge.SSE().feed(b"data: not json\n\n")
        with self.assertRaises(activity.Unavailable):
            bridge.SSE().feed(b"x" * (1024 * 1024 + 1))

    def test_clock_only_changes_do_not_trigger_busy_reconnect(self):
        first = snapshot()
        second = copy.deepcopy(first)
        second["t1"]["meta"].update(durationMs=9999, durationAt=9999)
        self.assertEqual(bridge.fingerprint(first), bridge.fingerprint(second))
        second["t1"]["meta"]["contextTokens"] = 272000
        self.assertNotEqual(bridge.fingerprint(first), bridge.fingerprint(second))

    def test_cache_age_is_not_a_visible_change_and_events_are_sanitized(self):
        first, second = snapshot(), snapshot()
        first["t1"]["meta"]["metricsCache"] = {"at": 100}
        second["t1"]["meta"]["metricsCache"] = {"at": 999}
        self.assertEqual(bridge.fingerprint(first), bridge.fingerprint(second))
        self.assertEqual(bridge.invalidations([
            {"type": "session.text.delta", "data": {"text": "private"}},
            {"type": "session.usage.updated", "data": {"sessionID": "ses_1", "prompt": "private"}},
            {"type": "location.created", "data": {"directory": "private"}},
        ]), [{"type": "session.usage.updated", "data": {"sessionID": "ses_1"}},
             {"type": "location.created", "data": {"sessionID": None}}])

    def test_packet_sends_relative_clock_without_mutating_state(self):
        original = snapshot()
        result = bridge.packet(original, "key", True, "event", now=5000)
        self.assertEqual(result["snapshot"]["t1"]["meta"]["durationMs"], 5000)
        self.assertEqual(result["snapshot"]["t1"]["meta"]["durationAt"], 0)
        self.assertEqual(original["t1"]["meta"]["durationMs"], 1000)
        self.assertTrue(result["snapshot"]["t1"]["eventBridge"])


class FollowTests(unittest.TestCase):
    def run_follow(self, snapshots, events=(), previous=None, targets=None):
        order = []
        self.collected_targets = []
        self.dirty_at_collect = []
        class Sock:
            def settimeout(self, seconds):
                assert seconds > 0
            def fileno(self):
                return 0
        class Response:
            def __init__(self):
                self.chunks = iter(events)
            def read1(self, limit):
                order.append("read")
                try:
                    return next(self.chunks)
                except StopIteration:
                    raise TimeoutError()
        @contextmanager
        def subscribe(api):
            order.append("subscribe")
            try:
                yield Response(), Sock()
            finally:
                order.append("close")
        values = iter(snapshots)
        def collect(api, targets):
            order.append("snapshot")
            self.collected_targets.append(copy.deepcopy(targets))
            self.dirty_at_collect.append(copy.deepcopy(getattr(api, "dirty_sessions", set())))
            return next(values)
        api = type("API", (), {})()
        api.shell_cache = {"at": 100, "items": []}
        self.api = api
        with patch.object(bridge, "subscribe", subscribe), patch.object(bridge, "collect", collect), \
                patch.object(bridge.select, "select", side_effect=[([0], [], [])] * max(0, len(events) - 1) + [([], [], [])] * 20):
            result = bridge.follow(api, targets or [], previous)
        return result, order

    def test_initial_snapshot_subscribes_first_and_closes(self):
        result, order = self.run_follow([snapshot()])
        self.assertEqual(order, ["subscribe", "snapshot", "close"])
        self.assertEqual(result["reason"], "reconcile")

    def test_event_wakes_immediately_not_at_lease_timeout(self):
        first = snapshot()
        event = b'data: {"type":"session.step.ended"}\n\n'
        result, order = self.run_follow([first, snapshot(shells=1)], [event], bridge.fingerprint(first))
        self.assertEqual(result["reason"], "event")
        self.assertEqual(result["snapshot"]["t1"]["shells"], 1)
        self.assertEqual(order, ["subscribe", "snapshot", "read", "snapshot", "close"])

    def test_text_deltas_and_connect_notification_do_not_query(self):
        first = snapshot()
        events = [b'data: {"type":"session.text.delta"}\n\n', b'data: {"type":"server.connected"}\n\n']
        result, order = self.run_follow([first], events, bridge.fingerprint(first))
        self.assertEqual(result["reason"], "renew")
        self.assertEqual(order.count("snapshot"), 1)

    def test_reconnect_reconciles_events_missed_between_connections(self):
        first = snapshot()
        result, order = self.run_follow([snapshot(shells=2)], previous=bridge.fingerprint(first))
        self.assertEqual(result["reason"], "reconcile")
        self.assertNotIn("read", order)

    def test_missing_session_keeps_healthy_stream_without_spinning(self):
        missing = {"t1": {"error": "Missing session"}}
        result, _ = self.run_follow([missing])
        self.assertTrue(result["connected"])
        result, order = self.run_follow([missing], previous=result["key"])
        self.assertEqual(result["reason"], "renew")
        self.assertEqual(order.count("snapshot"), 1)

    def test_creation_resolves_missing_session_on_event_not_lease_timeout(self):
        missing = {"t1": {"error": "Missing session"}}
        event = b'data: {"type":"session.created"}\n\n'
        result, _ = self.run_follow([missing, snapshot()], [event], bridge.fingerprint(missing))
        self.assertEqual(result["reason"], "event")
        self.assertEqual(result["snapshot"]["t1"]["sessionID"], "ses_1")

    def test_model_effort_context_and_lifecycle_events_publish_changed_metadata(self):
        for kind in ["session.model.selected", "session.agent.selected", "session.metadata.updated",
                     "session.usage.updated", "session.step.ended", "session.compaction.started",
                     "session.compaction.ended", "session.revert.committed", "session.renamed"]:
            with self.subTest(kind=kind):
                first = snapshot()
                changed = copy.deepcopy(first)
                changed["t1"]["meta"].update(modelID="new-model", variant="high", contextTokens=12345)
                event = ("data: " + json.dumps({"type": kind}) + "\n\n").encode()
                result, order = self.run_follow([first, changed], [event], bridge.fingerprint(first))
                self.assertEqual(result["reason"], "event")
                self.assertEqual(result["snapshot"]["t1"]["meta"]["variant"], "high")
                self.assertEqual(result["snapshot"]["t1"]["meta"]["contextTokens"], 12345)
                self.assertEqual(order.count("snapshot"), 2)

    def test_eof_is_failure_not_successful_renewal(self):
        with self.assertRaises(activity.Unavailable):
            first = snapshot()
            self.run_follow([first], [b""], bridge.fingerprint(first))

    def test_burst_is_one_rebuild_and_shell_events_invalidate_cache(self):
        first = snapshot()
        first["t1"]["meta"]["metricsCache"] = {"at": 100}
        first["t1"]["meta"]["durationState"] = {"totalMs": 500}
        events = [b'data: {"type":"session.status"}\n\n',
                  b'data: {"type":"session.shell.ended"}\n\n',
                  b'data: {"type":"session.usage.updated","data":{"sessionID":"ses_1"}}\n\n']
        result, order = self.run_follow([first, snapshot(shells=1)], events, bridge.fingerprint(first),
                                        targets=[{"address": "t1", "title": "Task"}])
        self.assertEqual(result["reason"], "event")
        self.assertEqual(order.count("snapshot"), 2)
        self.assertIsNone(self.api.shell_cache)
        # Snapshot state advances within a single lease, not just between execs.
        self.assertEqual(self.collected_targets[1][0]["durationState"], {"totalMs": 500})
        self.assertEqual(self.collected_targets[1][0]["metricsCache"], {"at": 100})
        self.assertEqual(self.dirty_at_collect, [set(), {"ses_1"}])

    def test_unchanged_batch_keeps_listening_without_rebuilding_text_deltas(self):
        first = snapshot()
        events = [b'data: {"type":"session.status"}\n\n',
                  b'data: {"type":"session.text.delta"}\n\n']
        result, order = self.run_follow([first, first], events, bridge.fingerprint(first))
        self.assertEqual(result["reason"], "renew")
        self.assertEqual(order.count("snapshot"), 2)


if __name__ == "__main__":
    unittest.main()
