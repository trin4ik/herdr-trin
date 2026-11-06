import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const source = fs.readFileSync(new URL("../mods/trin-agents/activity.js", import.meta.url), "utf8")
  .replace(/^import .*;\n/, "").replace("export class ActivityMonitor", "class ActivityMonitor")
  + "\nthis.Monitor = ActivityMonitor;";
const clockSource = fs.readFileSync(new URL("../mods/trin-agents/metrics.js", import.meta.url), "utf8");
const { WorkingClock } = await import(`data:text/javascript,${encodeURIComponent(clockSource)}`);

function fixture(extra = {}) {
  let now = 1000;
  const context = { Date: { now: () => now }, ...extra };
  vm.runInNewContext(source, context);
  return { monitor: new context.Monitor(), advance: ms => now += ms };
}
const agent = { address: "t1", title: "OC | Task" };
const live = { title: agent.title, subagents: 0, shells: 1 };
const failure = { title: agent.title, error: "Temporarily unavailable" };

test("first failure produces no question mark or count", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: failure });
  assert.equal(monitor.get(agent), null);
  assert.match(monitor.notice(agent), /unavailable.*not zero/);
});

test("transient error keeps a marked last known positive count", () => {
  const { monitor, advance } = fixture();
  monitor.accept({ t1: live });
  advance(5000);
  monitor.accept({ t1: failure });
  assert.equal(monitor.get(agent).shells, 1);
  assert.equal(monitor.get(agent).stale, true);
  assert.match(monitor.notice(agent), /Last known/);
});

test("a long outage marks old counts stale without removing the icon", () => {
  const { monitor, advance } = fixture();
  monitor.accept({ t1: live });
  advance(5000);
  monitor.accept({ t1: failure });
  advance(120000);
  monitor.accept({ t1: failure });
  assert.equal(monitor.get(agent).shells, 1);
  assert.equal(monitor.get(agent).stale, true);
});

test("two successful zeros clear the badge, one does not", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: live });
  monitor.accept({ t1: { ...live, shells: 0 } });
  assert.equal(monitor.get(agent).shells, 1);
  assert.match(monitor.notice(agent), /Confirming/);
  monitor.accept({ t1: { ...live, shells: 0 } });
  assert.equal(monitor.get(agent), null);
  assert.equal(monitor.notice(agent), "");
});

test("cached counts cannot follow a different session title", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: live });
  monitor.accept({ t1: { ...failure, title: "OC | Other" } });
  assert.equal(monitor.get({ ...agent, title: "OC | Other" }), null);
});

test("identical data does not redraw and removed agents are pruned", () => {
  const { monitor, advance } = fixture();
  assert.equal(monitor.accept({ t1: live }), true);
  advance(5000);
  assert.equal(monitor.accept({ t1: live }), false);
  monitor.accept({});
  assert.equal(monitor.get(agent).shells, 1);
  monitor.prune(new Set());
  assert.equal(monitor.get(agent), null);
  assert.equal(Object.keys(monitor.good).length, 0);
});

test("focus/poll suspension cannot expire a badge during a render", () => {
  const { monitor, advance } = fixture();
  monitor.accept({ t1: live });
  advance(120000);
  assert.equal(monitor.get(agent).shells, 1);
  assert.equal(monitor.get(agent).stale, true);
  assert.match(monitor.notice(agent), /refresh is paused/);
  monitor.accept({ t1: live });
  assert.equal(monitor.get(agent).stale, false);
});

test("an unrelated machine response cannot erase a cached badge", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: live });
  monitor.accept({ remote: { title: "Other", error: "SSH failed" } });
  assert.equal(monitor.get(agent).shells, 1);
  assert.equal(monitor.get(agent).stale, false);
});

test("alternating empty snapshots do not make task icons blink", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: live });
  for (let n = 0; n < 10; n++) {
    monitor.accept({ t1: { ...live, shells: 0 } });
    assert.equal(monitor.get(agent).shells, 1);
    monitor.accept({ t1: live });
    assert.equal(monitor.get(agent).shells, 1);
  }
});

test("an error is not a second confirmation of zero", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: live });
  monitor.accept({ t1: { ...live, shells: 0 } });
  monitor.accept({ t1: failure });
  monitor.accept({ t1: { ...live, shells: 0 } });
  assert.equal(monitor.get(agent).shells, 1);
  monitor.accept({ t1: { ...live, shells: 0 } });
  assert.equal(monitor.get(agent), null);
});

test("subagent and shell completion are confirmed independently", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: { ...live, subagents: 2 } });
  monitor.accept({ t1: live });
  assert.equal(monitor.get(agent).subagents, 2);
  monitor.accept({ t1: { ...live, shells: 0 } });
  assert.equal(monitor.get(agent).subagents, 0);
  assert.equal(monitor.get(agent).shells, 1);
});

test("a different session ID does not inherit completion grace", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: { ...live, sessionID: "ses_first" } });
  monitor.accept({ t1: { ...live, sessionID: "ses_second", shells: 0 } });
  assert.equal(monitor.get(agent), null);
});

test("a pending SSH bridge does not delay local publication or recovery", async () => {
  let releaseRemote;
  const waitRemote = new Promise(resolve => releaseRemote = resolve);
  const remote = { address: "remote/t1", title: "Remote", app: "opencode", space: "remote" };
  const requests = [];
  const list = [{ ...agent, app: "opencode", space: "local" }, remote];
  const { monitor, advance } = fixture({
    agents: () => list,
    spaces: () => [{ id: "local", machine: null }, { id: "remote", machine: "ssh-host" }],
    panes: () => [],
    terminals: () => [],
    readTextFile: async () => "python reader",
    appHome: () => "/test",
    exec: async (_command, _args, options) => {
      requests.push(options.on);
      if (options.on) {
        await waitRemote;
        return { code: 0, stdout: JSON.stringify({ connected: false, snapshot: { [remote.address]: { ...live, title: remote.title } } }) };
      }
      return { code: 0, stdout: JSON.stringify({ connected: false, snapshot: { [agent.address]: live } }) };
    },
  });
  const tasks = [];
  const cx = { spawn: fn => tasks.push(fn(cx)), notify() {} };
  monitor.refresh(cx);
  await tasks[0];
  assert.equal(monitor.get(agent).shells, 1);
  assert.equal(monitor.busy.has("ssh-host"), true);
  advance(5000);
  monitor.refresh(cx);
  await tasks[2];
  assert.equal(requests.filter(on => on === null).length, 2);
  assert.equal(requests.filter(on => on === "ssh-host").length, 1);
  releaseRemote();
  await tasks[1];
  assert.equal(monitor.get(remote).shells, 1);
  assert.equal(monitor.get(agent).shells, 1);
});

test("automatic session titles lose OC prefix but explicit names are preserved", () => {
  const main = fs.readFileSync(new URL("../mods/trin-agents/main.js", import.meta.url), "utf8");
  const fn = new Function(`${main.match(/function sessionName\(tab, agent\) \{[\s\S]*?\n\}/)[0]}; return sessionName;`)();
  assert.equal(fn({ named: false }, { title: "OC | Реальное имя" }), "Реальное имя");
  assert.equal(fn({ named: false }, { title: "OpenCode | Name | detail" }), "Name | detail");
  assert.equal(fn({ named: true, name: "OC | My explicit name" }, { title: "OC | Other" }), "OC | My explicit name");
});

test("session metadata remains available with zero task badges", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: { ...live, shells: 0, meta: { modelID: "gpt-6.1-sol-fast", contextTokens: 272000, durationReady: false } } });
  assert.equal(monitor.get(agent), null);
  assert.equal(monitor.details(agent).contextTokens, 272000);
  assert.equal(monitor.details(agent).durationReady, false);
});

test("a timing API failure preserves known time but freezes its live clock", () => {
  const { monitor } = fixture();
  monitor.accept({ t1: { ...live, meta: { durationReady: true, durationMs: 60000, durationAt: 1000, durationRunning: true } } });
  monitor.accept({ t1: { ...live, meta: { modelID: "New selection", durationError: "History unavailable" } } });
  const info = monitor.details(agent);
  assert.equal(info.modelID, "New selection");
  assert.equal(info.durationMs, 60000);
  assert.equal(info.durationRunning, false);
  assert.equal(info.durationStale, true);
});

function bridgeFixture(exec, extra = {}) {
  const state = fixture({
    agents: () => [{ ...agent, app: "opencode", space: "local" }],
    spaces: () => [{ id: "local", machine: null }], panes: () => [], terminals: () => [],
    readTextFile: async path => path.endsWith("bridge.py") ? "# event bridge" : "# snapshot reader",
    appHome: () => "/test", exec, ...extra,
  });
  const tasks = [];
  const timers = [];
  let notifications = 0;
  const cx = { spawn: fn => tasks.push(fn(cx)), notify: () => notifications++, timer: {
    after: (ms, fn) => {
      const timer = { ms, fn, cancelled: false, cancel() { this.cancelled = true; } };
      timers.push(timer);
      return timer;
    },
  } };
  return { ...state, tasks, cx, timers, tick(ms = 1000) {
    state.advance(ms);
    const pending = timers.splice(0);
    for (const timer of pending) if (!timer.cancelled) {
      timer.ms -= ms;
      if (timer.ms <= 0) timer.fn(cx); else timers.push(timer);
    }
  }, notifications: () => notifications };
}

test("healthy event bridge reconnects itself, reuses key and never duplicates an in-flight subscription", async () => {
  const calls = [];
  let release;
  const pending = new Promise(resolve => release = resolve);
  let started;
  const secondStarted = new Promise(resolve => started = resolve);
  const packet = { connected: true, key: "fingerprint", snapshot: { t1: { ...live, eventBridge: true,
    meta: { durationReady: true, durationMs: 1000, durationAt: 0, durationRunning: true } } } };
  const { monitor, cx, tasks, tick } = bridgeFixture(async (_cmd, args) => {
    calls.push(args);
    if (calls.length > 1) { started(); await pending; }
    return { code: 0, stdout: JSON.stringify(packet) };
  });
  monitor.refresh(cx);
  monitor.refresh(cx);
  await tasks[0];
  assert.equal(calls.length, 1);
  tick();
  await secondStarted;
  assert.equal(calls.length, 2);
  assert.match(calls[0][1], /trin_activity/);
  assert.match(calls[0][1], /# event bridge/);
  assert.equal(JSON.parse(calls[1][3]), "fingerprint");
  assert.equal(monitor.details(agent).durationAt, 1000);
  monitor.close();
  release();
  await tasks[1];
  assert.equal(calls.length, 2);
});

test("bridge failure backs off instead of tight-looping snapshots", async () => {
  let calls = 0;
  const { monitor, cx, tasks, advance } = bridgeFixture(async () => {
    calls++;
    return { code: 0, stdout: JSON.stringify({ connected: false, key: null, snapshot: { t1: live } }) };
  });
  monitor.refresh(cx);
  await tasks[0];
  monitor.refresh(cx);
  assert.equal(calls, 1);
  assert.equal(monitor.get(agent).shells, 1);
  advance(4001);
  monitor.refresh(cx);
  await tasks[1];
  assert.equal(calls, 2);
  assert.ok(monitor.retryAt.get(null) > 5001);
  monitor.close();
});

test("fast healthy replies are rate limited and close cancels reconnect", async () => {
  let calls = 0;
  const state = { identity: "hub", shells: { at: 1, items: [] } };
  const { monitor, cx, tasks, timers, tick } = bridgeFixture(async (_cmd, args) => {
    calls++;
    if (calls > 1) {
      assert.deepEqual(JSON.parse(args[4]), state);
      assert.equal(JSON.parse(args[2])[0].metricsCache.version, "v");
    }
    return { code: 0, stdout: JSON.stringify({ connected: true, key: "key", state,
      snapshot: { t1: { ...live, meta: { metricsCache: { version: "v" } } } } }) };
  });
  monitor.refresh(cx);
  await tasks[0];
  for (let n = 0; n < 20; n++) monitor.refresh(cx);
  assert.equal(calls, 1);
  assert.equal(timers.length, 1);
  tick(999);
  assert.equal(calls, 1);
  tick(1);
  await tasks[1];
  assert.equal(calls, 2);
  monitor.close();
  tick(2000);
  assert.equal(calls, 2);
  assert.equal(monitor.bridgeStates.size, 0);
});

test("transport failure drops bridge state rather than reusing a disconnected shell cache", async () => {
  const { monitor, cx, tasks } = bridgeFixture(async () => { throw new Error("offline"); });
  monitor.bridgeStates.set(null, { identity: "hub", shells: { at: 1, items: [] } });
  monitor.refresh(cx);
  await tasks[0];
  assert.equal(monitor.bridgeStates.has(null), false);
  assert.equal(monitor.failures.get(null), 1);
  monitor.close();
});

test("closed mod discards a late bridge response and cannot reconnect", async () => {
  let release;
  const pending = new Promise(resolve => release = resolve);
  const { monitor, cx, tasks, notifications } = bridgeFixture(async () => {
    await pending;
    return { code: 0, stdout: JSON.stringify({ connected: true, key: "key", snapshot: { t1: live } }) };
  });
  monitor.refresh(cx);
  monitor.close();
  release();
  await tasks[0];
  assert.equal(monitor.get(agent), null);
  assert.equal(notifications(), 0);
  monitor.refresh(cx);
  assert.equal(tasks.length, 1);
});

test("healthy SSE lease does not expire at old 20s polling threshold", () => {
  const { monitor, advance } = fixture();
  monitor.accept({ t1: { ...live, eventBridge: true } });
  advance(25000);
  assert.equal(monitor.get(agent).stale, false);
  advance(20001);
  assert.equal(monitor.get(agent).stale, true);
});

test("a new session supersedes the pending lease immediately and late old data cannot overwrite it", async () => {
  let list = [{ ...agent, app: "opencode", space: "local" }];
  const second = { ...list[0], address: "t2", title: "New session" };
  const releases = [], calls = [];
  let startFirst, startSecond;
  const firstStarted = new Promise(resolve => startFirst = resolve);
  const secondStarted = new Promise(resolve => startSecond = resolve);
  const { monitor, cx, tasks, tick } = bridgeFixture(async (_cmd, args) => {
    calls.push(JSON.parse(args[2]));
    const value = new Promise(resolve => releases.push(resolve));
    (calls.length === 1 ? startFirst : startSecond)();
    return value;
  }, { agents: () => list });
  monitor.refresh(cx);
  await firstStarted;
  list = [...list, second];
  monitor.refresh(cx);
  tick();
  await secondStarted;
  assert.equal(calls.length, 2);
  assert.equal(calls[1].length, 2);
  const meta = { modelID: "selected", variant: "high", contextTokens: 123 };
  releases[1]({ code: 0, stdout: JSON.stringify({ connected: false, key: "new", snapshot: {
    t1: live, t2: { ...live, title: second.title, meta },
  } }) });
  await tasks[1];
  assert.equal(monitor.details(second).variant, "high");
  releases[0]({ code: 0, stdout: JSON.stringify({ connected: true, key: "obsolete", snapshot: { t1: failure } }) });
  await tasks[0];
  assert.equal(monitor.get(agent).stale, false);
  assert.equal(monitor.bridgeKeys.get(null), "new");
  assert.equal(calls.length, 2);
  monitor.close();
});

test("selected model, effort and context update without a status or title change", () => {
  const { monitor } = fixture();
  const info = { ...live, shells: 0, meta: { modelID: "first", variant: "medium", contextTokens: null } };
  monitor.accept({ t1: info });
  assert.equal(monitor.details(agent).modelID, "first");
  assert.equal(monitor.details(agent).contextTokens, null);
  assert.equal(monitor.accept({ t1: { ...info, meta: { modelID: "second", variant: "high", contextTokens: 24000 } } }), true);
  assert.equal(monitor.details(agent).modelID, "second");
  assert.equal(monitor.details(agent).variant, "high");
  assert.equal(monitor.details(agent).contextTokens, 24000);
  assert.equal(monitor.get(agent), null);
});

test("rapid target changes are bounded to two calls and coalesce to the latest list", async () => {
  let title = "First", count = 0;
  const releases = [], signals = [];
  const started = [0, 1, 2].map(() => new Promise(resolve => signals.push(resolve)));
  const { monitor, cx, tasks, tick } = bridgeFixture(async () => {
    const index = count++;
    const result = new Promise(resolve => releases.push(resolve));
    signals[index]();
    return result;
  }, { agents: () => [{ ...agent, title, app: "opencode", space: "local" }] });
  monitor.refresh(cx);
  await started[0];
  title = "Second";
  monitor.refresh(cx);
  tick();
  await started[1];
  title = "Third";
  monitor.refresh(cx);
  title = "Latest";
  monitor.refresh(cx);
  assert.equal(count, 2);
  releases[0]({ code: 0, stdout: JSON.stringify({ connected: true, key: "old", snapshot: {} }) });
  await tasks[0];
  tick();
  await started[2];
  assert.equal(count, 3);
  assert.equal(monitor.inFlight.get(null), 2);
  assert.match(monitor.requests.get(null).signature, /Latest/);
  monitor.close();
  releases[1]({ code: 1 });
  releases[2]({ code: 1 });
  await Promise.all(tasks);
});

test("removing a terminal during its lease never resurrects its late metadata", async () => {
  let list = [{ ...agent, app: "opencode", space: "local" }], release;
  const pending = new Promise(resolve => release = resolve);
  const { monitor, cx, tasks } = bridgeFixture(async () => {
    await pending;
    return { code: 0, stdout: JSON.stringify({ connected: true, key: "old", snapshot: { t1: live } }) };
  }, { agents: () => list });
  monitor.refresh(cx);
  list = [];
  monitor.refresh(cx);
  release();
  await tasks[0];
  assert.equal(monitor.get(agent), null);
  assert.equal(Object.keys(monitor.results).length, 0);
  assert.equal(monitor.busy.size, 0);
});

test("subagent tooltip details follow the same two-zero confirmation as their badge", () => {
  const { monitor } = fixture();
  const child = { title: "Child", modelID: "model", totalTokens: 100 };
  monitor.accept({ t1: { ...live, shells: 0, subagents: 1, subagentsDirect: 1, subagentsNested: 0, subagentDetails: [child] } });
  const empty = { ...live, shells: 0, subagents: 0, subagentsDirect: 0, subagentsNested: 0, subagentDetails: [] };
  monitor.accept({ t1: empty });
  assert.equal(monitor.get(agent).subagentsDirect, 1);
  assert.equal(monitor.get(agent).subagentDetails[0].title, "Child");
  monitor.accept({ t1: empty });
  assert.equal(monitor.get(agent), null);
});

test("rename metadata gap preserves WorkingClock until a genuinely different session is confirmed", () => {
  const { monitor } = fixture();
  const clock = new WorkingClock();
  const original = { ...agent, status: "working" };
  const renamed = { ...original, title: "Renamed" };
  const meta = { durationReady: true, durationMs: 10000 };
  monitor.accept({ t1: { ...live, sessionID: "ses_first", meta } });
  clock.sync([original], a => monitor.details(a), 0);
  clock.sync([original], a => monitor.details(a), 5000);
  assert.equal(clock.value("t1"), 15000);
  assert.equal(monitor.details(renamed), null);
  clock.sync([renamed], a => monitor.details(a), 6000);
  assert.equal(clock.value("t1"), 16000);
  monitor.accept({ t1: { ...live, title: renamed.title, sessionID: "ses_first", meta } });
  clock.sync([renamed], a => monitor.details(a), 7000);
  assert.equal(clock.value("t1"), 17000);
  monitor.accept({ t1: { ...live, title: renamed.title, sessionID: "ses_second", meta: { ...meta, durationMs: 500 } } });
  clock.sync([renamed], a => monitor.details(a), 8000);
  assert.equal(clock.value("t1"), 500);
});
