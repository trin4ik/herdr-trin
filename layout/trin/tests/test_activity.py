"""Fixture checks for task attribution; no server or database is touched."""

import importlib.util
from pathlib import Path
import sys
import unittest
import copy
import json
import ssl
from unittest.mock import patch, MagicMock

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("activity", Path(__file__).parents[1] / "mods/trin-agents/activity.py")
activity = importlib.util.module_from_spec(spec)
spec.loader.exec_module(activity)


class FakeAPI:
    def __init__(self, sessions, active=(), shells=None):
        self.sessions = {s["id"]: s for s in sessions}
        self.active = {sid: {"type": "running"} for sid in active}
        self.shells = shells or {"/project": []}

    def get(self, path, **query):
        if path == "/api/session":
            return {"data": [s for s in self.sessions.values() if query["search"] in s.get("title", "")]}
        if path == "/api/session/active":
            return {"data": self.active}
        if path.startswith("/api/session/"):
            return {"data": self.sessions[path.split("/")[-1]]}
        if path == "/api/debug/location":
            return [{"directory": d} for d in self.shells]
        if path == "/api/shell":
            return {"data": self.shells[query["location[directory]"]]}
        raise AssertionError(path)


def session(id, title="Task", parent=None, directory="/project"):
    return {"id": id, "title": title, "parentID": parent, "location": {"directory": directory}}


def shell(id, owner, status="running"):
    return {"id": id, "status": status, "metadata": {"sessionID": owner}}


TARGET = {"address": "t1", "title": "OC | Task", "cwd": "/project"}


class TransportTests(unittest.TestCase):
    def test_http_constructs_no_ssl_or_proxy_handler_and_reuses_opener(self):
        state = json.dumps({"url": "http://127.0.0.1:1234", "pid": 42, "password": "fixture-secret"})
        with patch.object(activity.Path, "read_text", return_value=state), \
                patch.object(activity.API, "get", return_value={"pid": 42}), \
                patch.object(ssl, "create_default_context", side_effect=AssertionError("SSL on HTTP")), \
                patch.object(activity.urllib.request, "ProxyHandler", side_effect=AssertionError("proxy")):
            api = activity.API()
        self.assertFalse(any(isinstance(h, activity.urllib.request.HTTPSHandler) for h in api.opener.handlers))
        self.assertTrue(any(isinstance(h, activity.NoRedirect) for h in api.opener.handlers))
        opener = api.opener
        response = MagicMock()
        response.__enter__.return_value.read.return_value = b'{"data": []}'
        with patch.object(opener, "open", return_value=response) as opened:
            api.get("/api/session")
            api.get("/api/session")
        self.assertIs(api.opener, opener)
        self.assertEqual(opened.call_count, 2)
        response.__enter__.return_value.read.assert_called_with(4 * 1024 * 1024 + 1)
        response.__enter__.return_value.read.return_value = b'x' * (4 * 1024 * 1024 + 1)
        with patch.object(opener, "open", return_value=response), self.assertRaises(activity.Unavailable):
            api.get("/api/session")
        self.assertIsNone(activity.NoRedirect().redirect_request(None, None, 302, "", {}, "http://elsewhere"))


class ActivityTests(unittest.TestCase):
    def result(self, api):
        return activity.collect(api, [TARGET])["t1"]

    def test_idle_root_can_have_a_running_shell(self):
        info = self.result(FakeAPI([session("root")], shells={"/project": [shell("s1", "root")]}))
        self.assertEqual((info["subagents"], info["shells"]), (0, 1))

    def test_main_session_is_not_a_subagent(self):
        self.assertEqual(self.result(FakeAPI([session("root")], ["root"]))["subagents"], 0)

    def test_nested_children_and_their_shells(self):
        api = FakeAPI(
            [session("root"), session("child", "Child", "root"), session("grandchild", "Grandchild", "child")],
            ["root", "child", "grandchild"],
            {"/project": [shell("s1", "root"), shell("s2", "grandchild")]},
        )
        info = self.result(api)
        self.assertEqual((info["subagents"], info["shells"]), (2, 2))
        self.assertEqual((info["shellsOwn"], info["shellsInherited"]), (1, 1))
        self.assertEqual(info["shellDetails"][1]["ownerTitle"], "Grandchild")
        self.assertTrue(info["shellDetails"][1]["inherited"])
        self.assertEqual((info["subagentsDirect"], info["subagentsNested"]), (1, 1))
        self.assertEqual({c["title"] for c in info["subagentDetails"]}, {"Child", "Grandchild"})
        self.assertTrue(next(c for c in info["subagentDetails"] if c["title"] == "Grandchild")["nested"])

    def test_other_sessions_do_not_leak_into_counts(self):
        api = FakeAPI([session("root"), session("other", "Other")], ["other"], {"/project": [shell("s1", "other")]})
        info = self.result(api)
        self.assertEqual((info["subagents"], info["shells"]), (0, 0))

    def test_finished_shells_and_subagents_are_excluded(self):
        api = FakeAPI([session("root"), session("child", "Child", "root")], shells={"/project": [shell("s1", "root", "exited")]})
        self.assertEqual(self.result(api)["shells"], 0)
        self.assertEqual(self.result(api)["subagents"], 0)

    def test_shell_ids_are_deduplicated_across_locations(self):
        item = shell("s1", "root")
        api = FakeAPI([session("root")], shells={"/project": [item], "/another": [item]})
        self.assertEqual(self.result(api)["shells"], 1)

    def test_unowned_shell_is_not_guessed(self):
        api = FakeAPI([session("root")], shells={"/project": [{"id": "s1", "status": "running", "metadata": {}}]})
        self.assertEqual(self.result(api)["shells"], 0)

    def test_missing_session_is_unknown_not_zero(self):
        self.assertIn("error", self.result(FakeAPI([])))

    def test_duplicate_title_and_directory_is_unknown(self):
        self.assertIn("error", self.result(FakeAPI([session("one"), session("two")])))

    def test_directory_disambiguates_equal_titles(self):
        api = FakeAPI([session("one"), session("two", directory="/another")])
        self.assertEqual(self.result(api)["sessionID"], "one")

    def test_unique_title_survives_a_changed_location(self):
        self.assertEqual(self.result(FakeAPI([session("root", directory="/moved")]))["sessionID"], "root")

    def test_valid_osc_hint_disambiguates(self):
        api = FakeAPI([session("one"), session("two")])
        matched = activity.match_session(api, {**TARGET, "hints": ["two"]})
        self.assertEqual(matched["id"], "two")

    def test_stale_osc_title_does_not_match(self):
        api = FakeAPI([session("old", "Old task"), session("new")])
        self.assertEqual(activity.match_session(api, {**TARGET, "hints": ["old"]})["id"], "new")

    def test_ellipsized_terminal_title_matches_full_session_title(self):
        full = "Проверка Alpine-контейнеров и пароля в 256 символов"
        api = FakeAPI([session("root", full, directory="/moved")])
        target = {**TARGET, "title": "OC | Проверка Alpine-контейнеров и пароля …"}
        self.assertEqual(activity.match_session(api, target)["id"], "root")

    def test_ellipsized_title_never_guesses_between_equal_prefixes(self):
        prefix = "Проверка Alpine-контейнеров"
        api = FakeAPI([session("one", prefix + " A"), session("two", prefix + " B")])
        target = {**TARGET, "title": "OC | " + prefix + " …"}
        self.assertIsNone(activity.match_session(api, target))
        self.assertEqual(activity.match_session(api, {**target, "hints": ["two"]})["id"], "two")

    def test_literal_ellipsis_title_wins_over_prefix_candidates(self):
        title = "Проверка Alpine-контейнеров …"
        api = FakeAPI([session("exact", title), session("other", "Проверка Alpine-контейнеров extra")])
        self.assertEqual(activity.match_session(api, {**TARGET, "title": title})["id"], "exact")

    def test_short_or_non_ellipsized_prefixes_do_not_match(self):
        api = FakeAPI([session("other", "Task extra")])
        for title in ["Task", "Task …"]:
            self.assertIsNone(activity.match_session(api, {**TARGET, "title": title}))

    def test_parent_cycle_is_not_reported_as_zero(self):
        api = FakeAPI([session("root"), session("child", "Child", "child")], ["child"])
        with self.assertRaises(activity.Unavailable):
            self.result(api)

    def test_fork_is_not_a_subagent(self):
        fork = {**session("fork", "Fork"), "fork": {"sessionID": "root"}}
        self.assertEqual(self.result(FakeAPI([session("root"), fork], ["fork"]))["subagents"], 0)

    def test_prefix_only_is_stripped(self):
        self.assertEqual(activity.title_of("OC | Task | detail"), "Task | detail")

    def test_shell_of_finished_child_is_still_counted_and_identified(self):
        chrome = {**shell("s1", "child"), "command": "google-chrome --headless=new --secret=do-not-export"}
        api = FakeAPI([session("root"), session("child", "Scene S4", "root")], shells={"/project": [chrome]})
        info = self.result(api)
        self.assertEqual((info["subagents"], info["shellsOwn"], info["shellsInherited"]), (0, 0, 1))
        self.assertEqual(info["shellDetails"], [{"program": "google-chrome", "ownerTitle": "Scene S4", "inherited": True,
                                                "ownerActive": False, "pid": None}])
        self.assertNotIn("secret", str(info))

    def test_program_tooltip_never_exports_arguments_or_env_assignments(self):
        self.assertEqual(activity.program_of("/usr/bin/python3 --token=secret"), "python3")
        self.assertEqual(activity.program_of("API_KEY=secret python3 task.py"), "shell")
        self.assertEqual(activity.program_of(""), "shell")

    def test_one_lookup_failure_does_not_erase_other_sessions(self):
        class PartialAPI(FakeAPI):
            def get(self, path, **query):
                if query.get("search") == "Broken":
                    raise activity.Unavailable()
                return super().get(path, **query)
        api = PartialAPI([session("root")])
        result = activity.collect(api, [TARGET, {**TARGET, "address": "t2", "title": "OC | Broken"}])
        self.assertEqual(result["t1"]["shells"], 0)
        self.assertIn("error", result["t2"])


class HistoryAPI:
    def __init__(self, messages):
        self.messages = messages

    def get(self, path, **query):
        if path.endswith("/message"):
            offset = int(query.get("cursor", "0"))
            page = self.messages[offset:offset + 40]
            return {"data": page, "cursor": {"next": str(offset + len(page))}}
        return {"data": next(m for m in self.messages if m["id"] == path.split("/")[-1])}


def step(index, start, end=None):
    stamp = {"created": start}
    if end is not None:
        stamp["completed"] = end
    return {"id": f"msg_{index:06d}", "type": "assistant", "time": stamp}


class SessionMetricsTests(unittest.TestCase):
    def test_unchanged_idle_metrics_reuse_only_compact_cache(self):
        usage = {"input": 1, "output": 2, "reasoning": 0, "cache": {"read": 3, "write": 0}}
        class MetricsAPI:
            def __init__(self):
                self.messages = 0
            def get(self, path, **query):
                if path == "/api/model":
                    return {"data": []}
                self.messages += 1
                return {"data": [{**step(1, 100, 200), "tokens": usage,
                                  "parts": [{"text": "private prompt must never leave"}]}],
                        "cursor": {"next": "next"}}
        api = MetricsAPI()
        s = {**session("root"), "time": {"updated": 100}, "tokens": usage}
        first = activity.session_metrics(api, s, TARGET, False)
        target = {**TARGET, "durationState": first["durationState"], "metricsCache": first["metricsCache"]}
        api.messages = 0
        second = activity.session_metrics(api, s, target, False)
        self.assertEqual(api.messages, 0)
        self.assertEqual(second["contextTokens"], 6)
        self.assertNotIn("private prompt", json.dumps(second))
        api.dirty_sessions = {"root"}
        activity.session_metrics(api, s, target, False)
        self.assertGreater(api.messages, 0)
        api.dirty_sessions = set()
        api.messages = 0
        activity.session_metrics(api, {**s, "time": {"updated": 101}}, target, False)
        self.assertGreater(api.messages, 0)
        api.messages = 0
        expired = copy.deepcopy(target)
        expired["metricsCache"]["at"] -= 31
        activity.session_metrics(api, s, expired, False)
        self.assertGreater(api.messages, 0)

    def test_shell_cache_is_bounded_by_ttl_and_contains_no_arguments(self):
        class CountingAPI(FakeAPI):
            calls = 0
            def get(self, path, **query):
                if path in ("/api/debug/location", "/api/shell"):
                    self.calls += 1
                return super().get(path, **query)
        api = CountingAPI([session("root")], shells={"/project": [
            {**shell("s1", "root"), "command": "python3 --password=private-secret", "env": {"TOKEN": "secret"}}]})
        activity.collect(api, [TARGET])
        self.assertEqual(api.calls, 2)
        self.assertNotIn("secret", json.dumps(api.shell_cache))
        activity.collect(api, [TARGET])
        self.assertEqual(api.calls, 2)
        api.shell_cache["at"] -= 19
        activity.collect(api, [TARGET])
        self.assertEqual(api.calls, 4)
        api.shell_cache = None
        activity.collect(api, [TARGET])
        self.assertEqual(api.calls, 6)

    def test_duration_uses_tail_cursor_and_skips_user_messages(self):
        class TailAPI:
            queries = []
            def get(self, path, **query):
                self.queries.append(query)
                if query.get("cursor") == "tail":
                    return {"data": [], "cursor": {}}
                return {"data": [step(1, 1, 10), {**step(2, 20, 1000), "type": "user"}],
                        "cursor": {"next": "tail"}}
        api = TailAPI()
        first = activity.scan_duration(api, "root")
        second = activity.scan_duration(api, "root", first["durationState"])
        third = activity.scan_duration(api, "root", second["durationState"])
        self.assertEqual(third["durationMs"], 9)
        self.assertEqual([q.get("cursor") for q in api.queries], [None, "tail", "tail"])

    def test_tokens_include_all_usage_buckets(self):
        usage = {"input": 1, "output": 2, "reasoning": 3, "cache": {"read": 4, "write": 5}}
        self.assertEqual(activity.token_total(usage), 15)
        self.assertIsNone(activity.token_total(None))
        self.assertIsNone(activity.token_total({"input": 1}))

    def test_duration_excludes_idle_gaps(self):
        info = activity.scan_duration(HistoryAPI([step(1, 1000, 2000), step(2, 100000, 103000)]), "root", now=200000)
        self.assertTrue(info["durationReady"])
        self.assertEqual(info["durationMs"], 4000)

    def test_incomplete_history_never_presents_a_partial_total(self):
        api = HistoryAPI([step(i, i * 100, i * 100 + 10) for i in range(80)])
        first = activity.scan_duration(api, "root", now=100000)
        self.assertFalse(first["durationReady"])
        self.assertIsNone(first["durationMs"])
        second = activity.scan_duration(api, "root", first["durationState"], now=100000)
        self.assertTrue(second["durationReady"])
        self.assertEqual(second["durationMs"], 800)
        third = activity.scan_duration(api, "root", second["durationState"], now=100000)
        self.assertEqual(third["durationMs"], 800)

    def test_open_step_is_revisited_and_counted_only_once(self):
        api = HistoryAPI([step(1, 1000)])
        first = activity.scan_duration(api, "root", running=True, now=3000)
        self.assertEqual(first["durationMs"], 2000)
        api.messages[0]["time"]["completed"] = 4000
        second = activity.scan_duration(api, "root", first["durationState"], running=False, now=5000)
        self.assertEqual(second["durationMs"], 3000)
        third = activity.scan_duration(api, "root", second["durationState"], running=False, now=6000)
        self.assertEqual(third["durationMs"], 3000)
        self.assertFalse(third["durationRunning"])

    def test_abandoned_old_step_does_not_accrue_live_time(self):
        info = activity.scan_duration(HistoryAPI([step(1, 1000), step(2, 5000, 6000)]), "root", running=True, now=999999)
        self.assertEqual(info["durationMs"], 1000)
        self.assertFalse(info["durationRunning"])

    def test_timing_state_cannot_follow_a_different_session(self):
        old = activity.scan_duration(HistoryAPI([step(1, 1000, 9000)]), "first")
        other = activity.scan_duration(HistoryAPI([step(2, 100, 200)]), "second", old["durationState"])
        self.assertEqual(other["durationMs"], 100)

    def test_unavailable_history_does_not_hide_current_model(self):
        class BrokenAPI:
            def get(self, *args, **kwargs):
                raise activity.Unavailable()
        s = {**session("root"), "model": {"id": "gpt-6.1-sol-fast", "providerID": "openai"},
             "tokens": {"input": 10, "output": 20, "reasoning": 0, "cache": {"read": 100, "write": 0}}}
        info = activity.session_metrics(BrokenAPI(), s, TARGET, False)
        self.assertEqual(info["totalTokens"], 130)
        self.assertEqual(info["usage"], {"input": 10, "output": 20, "reasoning": 0, "cacheRead": 100, "cacheWrite": 0})
        self.assertIn("contextError", info)
        self.assertEqual(info["modelID"], "gpt-6.1-sol-fast")
        self.assertIn("durationError", info)

    def test_context_limit_matches_usage_model_not_just_current_selected_model(self):
        class ModelAPI:
            def get(self, path, **query):
                if path == "/api/model":
                    self.query = query
                    return {"data": [{"id": "old", "providerID": "openai", "limit": {"context": 1000000}},
                                     {"id": "new", "providerID": "openai", "limit": {"context": 600000}},
                                     {"id": "old", "providerID": "other", "limit": {"context": 200000}}]}
                if path.endswith("/message"):
                    return {"data": [{**step(1, 1, 2), "model": {"id": "old", "providerID": "openai"},
                                     "tokens": {"input": 167000, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}}]}
                raise activity.Unavailable()
        api = ModelAPI()
        info = activity.session_metrics(api, {**session("root"), "model": {"id": "new", "providerID": "openai"}}, TARGET, False)
        self.assertEqual(info["contextLimit"], 1000000)
        self.assertEqual(info["contextTokens"], 167000)
        self.assertEqual(info["contextModelID"], "old")
        self.assertEqual(api.query, {"location[directory]": "/project"})

    def test_new_session_shows_selected_model_and_effort_before_first_assistant_message(self):
        class EmptyAPI:
            def get(self, path, **query):
                if path == "/api/model":
                    return {"data": [{"id": "selected", "providerID": "openai", "limit": {"context": 1000000}}]}
                return {"data": []}
        for variant in ["medium", "high"]:
            s = {**session("root"), "model": {"id": "selected", "providerID": "openai", "variant": variant}}
            info = activity.session_metrics(EmptyAPI(), s, TARGET, False)
            self.assertEqual(info["modelID"], "selected")
            self.assertEqual(info["variant"], variant)
            self.assertEqual(info["contextLimit"], 1000000)
            self.assertIsNone(info["contextTokens"])

    def test_model_limit_cache_is_per_location_and_filters_invalid_limits(self):
        class ModelAPI:
            def __init__(self):
                self.calls = []
            def get(self, path, **query):
                self.calls.append(query)
                return {"data": [{"id": "model", "providerID": "openai", "limit": {"context": 1000 if query["location[directory]"] == "/project" else -1}}]}
        api, cache, model = ModelAPI(), {}, {"id": "model", "providerID": "openai"}
        self.assertEqual(activity.context_limit(api, session("one"), model, cache), 1000)
        self.assertEqual(activity.context_limit(api, session("two"), model, cache), 1000)
        self.assertIsNone(activity.context_limit(api, session("three", directory="/other"), model, cache))
        self.assertEqual(len(api.calls), 2)

    def test_subagent_details_are_bounded_without_truncating_the_count(self):
        children = [session(f"child_{i}", f"Task {i}", "root") for i in range(12)]
        info = activity.collect(FakeAPI([session("root"), *children], [c["id"] for c in children]), [TARGET])["t1"]
        self.assertEqual(info["subagents"], 12)
        self.assertEqual(len(info["subagentDetails"]), 8)

    def test_context_is_latest_usage_not_session_total(self):
        usage = {"input": 10, "output": 20, "reasoning": 0, "cache": {"read": 100, "write": 0}}
        class ContextAPI:
            def get(self, path, **query):
                self.query = query
                return {"data": [step(3, 300), {**step(2, 200, 250), "tokens": usage}]}
        api = ContextAPI()
        message = activity.context_message(api, {"id": "root", "tokens": {"input": 999999}})
        self.assertEqual(activity.token_total(message["tokens"]), 130)
        self.assertEqual(api.query["order"], "desc")

    def test_context_stops_at_completed_compaction(self):
        class ContextAPI:
            def get(self, *args, **kwargs):
                return {"data": [step(3, 300), {"id": "msg_000002", "type": "compaction", "status": "completed"},
                                 {**step(1, 100), "tokens": {"input": 99}}]}
        self.assertIsNone(activity.context_message(ContextAPI(), {"id": "root"}))

    def test_context_respects_revert_boundary_and_pagination(self):
        usage = {"input": 1, "output": 2, "reasoning": 3, "cache": {"read": 4, "write": 5}}
        class ContextAPI:
            def get(self, path, **query):
                if "cursor" not in query:
                    return {"data": [step(i, i) for i in range(100, 60, -1)], "cursor": {"next": "older"}}
                return {"data": [{**step(i, i), "tokens": usage} for i in range(60, 57, -1)]}
        message = activity.context_message(ContextAPI(), {"id": "root", "revert": {"messageID": "msg_000060"}})
        self.assertEqual(activity.token_total(message["tokens"]), 15)


if __name__ == "__main__":
    unittest.main()
