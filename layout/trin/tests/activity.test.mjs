import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const source = ["identity.js", "activity.js"].map(file =>
  fs.readFileSync(new URL(`../mods/trin-agents/${file}`, import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace(/^export /gm, "")).join("\n")
  + "\nthis.Monitor = ActivityMonitor; this.terminalSessionID = terminalSessionID;";
const clockSource = fs.readFileSync(new URL("../mods/trin-agents/metrics.js", import.meta.url), "utf8");
const { WorkingClock } = await import(`data:text/javascript,${encodeURIComponent(clockSource)}`);

function fixture(extra = {}, titles) {
  let now = 1000;
  const context = { Date: { now: () => now }, machines: () => [{ name: null, layout: "ready" }], ...extra };
  vm.runInNewContext(source, context);
  return { monitor: new context.Monitor(titles), terminalSessionID: context.terminalSessionID, advance: ms => now += ms };
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
    machines: () => [{ name: null, layout: "ready" }, { name: "ssh-host", layout: "ready" }],
    spaces: () => [{ id: "local", machine: null }, { id: "remote", machine: "ssh-host" }],
    panes: () => [],
    terminals: () => list.map(a => ({ address: a.address })),
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
  const fn = new Function(`${main.match(/function sessionName\(tab, agent, canonical\) \{[\s\S]*?\n\}/)[0]}; return sessionName;`)();
  assert.equal(fn({ named: false }, { title: "OC | Реальное имя" }), "Реальное имя");
  assert.equal(fn({ named: false }, { title: "OpenCode | Name | detail" }), "Name | detail");
  assert.equal(fn({ named: true, name: "OC | My explicit name" }, { title: "OC | Other" }), "OC | My explicit name");
  assert.equal(fn({ named: false }, { title: "OC | Other" }, "Canonical name"), "Canonical name");
  assert.equal(fn({ named: true, name: "User alias" }, { title: "OC | Other" }, "Canonical name"), "Canonical name");
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
  monitor.accept({ t1: { ...live, meta: { durationReady: true, durationMs: 60000, durationAt: 1000,
    durationRunning: true, contextTokens: 123, contextLimit: 456, modelID: "history", modelFromHistory: true } } });
  monitor.accept({ t1: { ...live, meta: { modelID: null, durationError: "History unavailable",
    contextError: "Context temporarily unavailable" } } });
  assert.equal(monitor.details(agent).modelID, "history");
  assert.equal(monitor.details(agent).modelStale, true);
  monitor.accept({ t1: { ...live, meta: { modelID: "New selection", durationError: "History unavailable",
    contextError: "Context temporarily unavailable" } } });
  const info = monitor.details(agent);
  assert.equal(info.modelID, "New selection");
  assert.equal(info.durationMs, 60000);
  assert.equal(info.durationRunning, false);
  assert.equal(info.durationStale, true);
  assert.equal(info.contextTokens, 123);
  assert.equal(info.contextLimit, 456);
  monitor.accept({ t1: { ...live, meta: { durationReady: false, contextTokens: null } } });
  assert.equal(monitor.details(agent).contextTokens, null, "a known context reset is not a read error");
});

function bridgeFixture(exec, extra = {}, titles) {
  const state = fixture({
    agents: () => [{ ...agent, app: "opencode", space: "local" }],
    spaces: () => [{ id: "local", machine: null }], panes: () => [], terminals: () => (extra.agents?.() ?? [agent]).map(a => ({ address: a.address })),
    readTextFile: async path => path.endsWith("bridge.py") ? "# event bridge" : "# snapshot reader",
    appHome: () => "/test", exec, ...extra,
  }, titles);
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

test("Herdr child terminals with panes are not OpenCode bridge targets", async () => {
  const root = { ...agent, app: "opencode", space: "local" };
  const child = { ...root, address: "t2", pane: "p2", title: "Child" };
  const calls = [];
  const { monitor, cx, tasks } = bridgeFixture(async (_cmd, args) => {
    calls.push(JSON.parse(args[2]).map(t => t.address));
    return { code: 0, stdout: JSON.stringify({ connected: false, snapshot: { t1: live } }) };
  }, { agents: () => [root, child], terminals: () => [{ address: root.address }, { address: "t2", parent: "t1" }] });
  monitor.refresh(cx);
  await tasks[0];
  assert.deepEqual(calls, [["t1"]]);
});

test("the OpenCode terminal report supplies the ID without waiting for the terminal title or bridge", async () => {
  const report = { app: "opencode", state: "working", title: "Long current title" };
  const terminal = { address: "t1", status: "done", report, records: {
    ses_main: report, ses_old: { ...report, state: "idle" },
  } };
  const calls = [];
  const { monitor, cx, tasks, terminalSessionID } = bridgeFixture(async (_cmd, args) => {
    calls.push(JSON.parse(args[2]));
    return { code: 0, stdout: JSON.stringify({ connected: false, snapshot: {
      t1: { ...live, sessionID: "ses_main", meta: { modelID: "confirmed" } },
    } }) };
  }, { terminals: () => [terminal] });
  assert.equal(terminalSessionID(terminal), "ses_main");
  assert.equal(terminalSessionID({ ...terminal, records: { ...terminal.records, ses_same: report } }), null);
  assert.equal(terminalSessionID({ ...terminal, report: { app: "claude" } }), null);
  assert.equal(terminalSessionID({ ...terminal, report: { ...report, title: "New unkeyed session" },
    records: { ses_old: report } }), null);
  monitor.refresh(cx);
  await tasks[0];
  assert.equal(calls[0][0].sessionID, "ses_main");
  const renamed = { ...agent, title: "renamed before the next bridge reply" };
  assert.equal(monitor.details(renamed, "ses_main").modelID, "confirmed");
  assert.equal(monitor.details(renamed, "ses_new"), null);
});

test("a proven binding survives idle without records, but not an unkeyed new turn or a different terminal process", () => {
  const { monitor } = fixture();
  const report = { app: "opencode", state: "working", title: "Task" };
  const terminal = { report, records: { ses_1: report }, foreground: { pid: 1 } };
  const row = { ...agent, app: "opencode", status: "working" };
  assert.equal(monitor.boundSessionID(row, terminal), "ses_1");
  terminal.records = {};
  terminal.report = { app: "opencode", state: "idle" };
  assert.equal(monitor.boundSessionID({ ...row, status: "idle" }, terminal), "ses_1");
  assert.equal(monitor.boundSessionID(row, terminal), null);
  const renamed = { ...row, status: "idle", title: "New name" };
  assert.equal(monitor.boundSessionID(renamed, terminal), null);
  assert.equal(monitor.boundSessionID(renamed, terminal, true), "ses_1");
  monitor.accept({ t1: { ...live, sessionID: "ses_1", sessionTitle: "New name" } });
  assert.equal(monitor.boundSessionID(renamed, terminal), "ses_1");
  monitor.accept({ t1: { ...live, sessionID: "ses_1", sessionTitle: "Second rename while label is unchanged" } });
  assert.equal(monitor.boundSessionID(renamed, terminal), "ses_1");
  terminal.foreground = { pid: 2 };
  assert.equal(monitor.boundSessionID(renamed, terminal), null);
});

test("an idle OpenCode tab launched with an explicit ID follows renames without a terminal record", () => {
  const { monitor } = fixture();
  const row = { ...agent, app: "opencode", status: "idle" };
  const terminal = { report: { app: "opencode", state: "idle" },
    foreground: { pid: 1, argv: ["/bin/opencode", "--session", "ses_1"] } };
  assert.equal(monitor.boundSessionID(row, terminal), "ses_1");
  monitor.accept({ t1: { ...live, sessionID: "ses_1", sessionTitle: "Renamed" } });
  assert.equal(monitor.session(row, "ses_1").sessionTitle, "Renamed");
  assert.equal(monitor.boundSessionID({ ...row, address: "t2" }, { ...terminal, foreground: {
    ...terminal.foreground, argv: [...terminal.foreground.argv, "--server", "other"],
  } }), null);
});

for (const reportCase of ["retained", "fresh", "reopened", "timestamp"]) test(`bridge confirmation survives repeated reads with a ${reportCase} replacement report`, async () => {
  const row = { ...agent, app: "opencode", status: "working", space: "remote" };
  let showAgent = true;
  const server = "http://127.0.0.1:1234";
  const terminal = { address: row.address, created_ms: 100, status_ms: 1000, foreground: {
    pid: 42, argv: ["opencode", "--server", server, "--auto", "--session", "ses_1"],
  } };
  const requests = [];
  const { monitor, cx, tasks, tick } = bridgeFixture((_cmd, args, opts) => new Promise(resolve => {
    requests.push({ targets: JSON.parse(args[2]), machine: opts.on, resolve });
  }), { agents: () => showAgent ? [row] : [], terminals: () => [terminal],
    spaces: () => [{ id: "remote", machine: "peer" }], machines: () => [{ name: "peer", layout: "ready" }] });
  const packet = { code: 0, stdout: JSON.stringify({ connected: true, snapshot: {
    t1: { ...live, sessionID: "ses_1", resumeVerified: true },
  } }) };
  monitor.bindings.set(row.address, { id: "ses_1", pid: 42, created: 100, caption: "Task" });
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), null);
  assert.ok(!monitor.resumeProof(row, terminal, "peer", "ses_1"));
  monitor.refresh(cx);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(requests[0].machine, "peer");
  assert.deepEqual(requests[0].targets[0].resumeProbe, { id: "ses_1", server });
  // Replace the process before the first reply. Its verification must fail.
  terminal.foreground.pid = 43;
  monitor.refresh(cx);
  requests[0].resolve(packet);
  await tasks[0];
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), null);
  tick();
  await new Promise(resolve => setImmediate(resolve));
  requests[1].resolve(packet);
  await tasks[1];
  assert.equal(monitor.boundSessionID(row, terminal, true, "peer"), "ses_1");
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), "ses_1");
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), "ses_1");
  assert.equal(monitor.resumeProof(row, terminal, "peer", "ses_1"), true);
  assert.ok(!monitor.resumeProof(row, terminal, "another-machine", "ses_1"));
  const report = { app: "opencode", state: "idle", title: "Other session" };
  assert.equal(monitor.boundSessionID(row, { ...terminal, report, records: { ses_other: report } }, false, "peer"), "ses_other");
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), null);
  showAgent = false;
  monitor.refresh(cx);
  showAgent = true;
  const undeclared = Object.assign(terminal, { report, records: { ses_unverified: report }, foreground: { pid: 43 } });
  assert.equal(monitor.boundSessionID(row, undeclared, false, "peer"), null, "a bound explicit client still requires the service check");
  assert.equal(monitor.explicitClient(row).requiresProbe, true);
  const unknownPID = { ...terminal, foreground: {} };
  assert.equal(monitor.boundSessionID(row, unknownPID, false, "peer"), null);
  const replacement = { ...terminal, foreground: { pid: 44 } };
  if (reportCase === "fresh") replacement.records = { ses_confirmed_default: report };
  else replacement.records = { ses_other: report };
  assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), null, "replacement must reveal its service first");
  assert.equal(monitor.explicitClient(row).clientPid, 44);
  assert.equal(monitor.explicitClient(row).awaitingLaunch, true);
  assert.equal(monitor.explicitClient(row).retiredReport, "ses_other", "retire the last report of the outgoing process");
  replacement.foreground.argv = reportCase === "retained" || reportCase === "timestamp"
    ? ["opencode", "--session", "ses_confirmed_default"] : ["opencode"];
  if (reportCase === "timestamp") {
    for (const stamp of [1000, undefined, 900]) {
      replacement.status_ms = stamp;
      assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), "ses_confirmed_default");
    }
    replacement.status_ms = 1001;
    assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), "ses_other");
  } else if (reportCase === "reopened") {
    assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), null);
    replacement.report = { ...report, state: "working", title: "Reopened session" };
    replacement.records = { ses_other: replacement.report };
    assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), "ses_other", "a changed report can reopen the same ID");
  } else {
    assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), "ses_confirmed_default",
      "the retained report must not override the replacement launch");
    assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), "ses_confirmed_default");
    replacement.records = { ses_confirmed_default: report };
    assert.equal(monitor.boundSessionID(row, replacement, false, "peer"), "ses_confirmed_default");
  }
  assert.equal(monitor.explicitClient(row)?.requiresProbe, false);
  assert.ok(monitor.explicitClient(row)?.lastReport);
  assert.equal(monitor.boundSessionID(row, { ...replacement, foreground: { ...replacement.foreground,
    argv: ["opencode", "--server", server] } }, false, "another"), null);
  monitor.close();
});

test("reload recovers a display-only rename through the same process hint without turning it into ownership", async () => {
  const row = { ...agent, app: "opencode", status: "idle", space: "remote" };
  const terminal = { address: row.address, created_ms: 50, foreground: {
    pid: 44, argv: ["opencode", "--server", "http://127.0.0.1:1234"],
  } };
  const hint = { presentationOnly: true, sessionID: "ses_prev", title: "", caption: row.title,
    pid: 44, created: 50, machine: "peer", requiresProbe: true };
  const sent = [];
  const { monitor, cx, tasks, tick } = bridgeFixture(async (_cmd, args) => {
    sent.push(JSON.parse(args[2])[0]);
    return { code: 0, stdout: JSON.stringify({ connected: true, snapshot: {
      t1: sent.length === 1 ? { ...failure, matchMissing: true }
        : { ...failure, sessionID: "ses_prev", sessionTitle: "Renamed", resumeVerified: true },
    } }) };
  }, { agents: () => [row], terminals: () => [terminal], spaces: () => [{ id: "remote", machine: "peer" }],
    machines: () => [{ name: "peer", layout: "ready" }] }, { [row.address]: hint });
  assert.equal(monitor.presentation(row), null);
  monitor.refresh(cx);
  await tasks[0];
  assert.equal(monitor.presentation(row), null);
  assert.equal(monitor.presentationHint(row).sessionID, "ses_prev");
  tick();
  await tasks[1];
  assert.deepEqual(sent.map(target => [target.sessionID, target.presentationID]), [[null, "ses_prev"], [null, "ses_prev"]]);
  assert.equal(monitor.presentation(row).sessionTitle, "Renamed");
  assert.equal(monitor.details(row), null, "title recovery does not require a successful metrics read");
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), null);
  terminal.foreground.argv = undefined;
  terminal.report = { app: "opencode", state: "idle", title: row.title };
  terminal.records = { ses_prev: terminal.report };
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), null, "a report does not identify the service");
  monitor.refresh(cx);
  tick();
  await tasks[2];
  assert.equal(sent[2].presentationID, undefined);
  assert.equal(sent[2].resumeProbe, undefined);
  assert.equal(sent[2].sessionID, null);
  assert.equal(sent[2].foreignService, true, "missing launch arguments cannot authorize the old hint");
  monitor.close();
});

test("unverified and malformed explicit server flags cannot use keyed or saved identities", () => {
  const report = { app: "opencode", state: "idle", title: "Task" };
  const row = { ...agent, app: "opencode", status: "idle" };
  for (const args of [["--server", "http://127.0.0.1:7777"], ["--server", "invalid"], ["--standalone"], ["--server"]]) {
    const { monitor } = fixture();
    const terminal = { created_ms: 100, report, records: { ses_1: report },
      foreground: { pid: 42, argv: ["opencode", "--session", "ses_1", ...args] } };
    monitor.bindings.set(row.address, { id: "ses_1", pid: 42, created: 100, caption: "Task" });
    assert.equal(monitor.boundSessionID(row, terminal), null);
  }
});

test("reload rechecks a confirmed idle client by saved ID, never by its possibly duplicated caption", async () => {
  const row = { ...agent, app: "opencode", status: "idle", space: "local" };
  const server = "http://fixture.invalid", sid = "ses_confirmed";
  const terminal = { address: row.address, created_ms: 51, foreground: { pid: 17, argv: ["opencode", "--server", server, "--auto"] },
    report: { app: "opencode", state: "idle", title: row.title } };
  const saved = { t1: { sessionID: sid, title: "Canonical", caption: row.title, created: 51, pid: 17,
    requiresProbe: true, machine: null, presentationOnly: false } };
  const requests = [];
  const { monitor, cx, tasks } = bridgeFixture(async (_cmd, args) => {
    requests.push(JSON.parse(args[2])[0]);
    return { code: 0, stdout: JSON.stringify({ connected: true, snapshot: { t1: { ...live,
      sessionID: sid, sessionTitle: "Canonical", sessionFolder: "/home", resumeVerified: true } } }) };
  }, { agents: () => [row], terminals: () => [terminal] }, saved);
  assert.equal(monitor.boundSessionID(row, terminal), null, "reload alone is not a service check");
  monitor.refresh(cx); await tasks[0];
  assert.deepEqual(requests[0].resumeProbe, { server, id: sid });
  assert.equal(monitor.boundSessionID(row, terminal), sid); assert.equal(monitor.session(row, sid).sessionFolder, "/home");
  assert.equal(monitor.bindings.get(row.address).proofCaption, row.title);
  assert.equal(monitor.boundSessionID({ ...row, status: "working" }, terminal), null);
  assert.ok(!monitor.resumeProof(row, terminal, null, sid));
  terminal.records = { ses_different: { app: "opencode", state: "working", title: "Earlier" } };
  assert.equal(monitor.boundSessionID(row, terminal), null);
  monitor.close();
});

test("a managed server without a terminal ID can show statistics and a title, but not claim a session", async () => {
  const row = { ...agent, app: "opencode", status: "idle", space: "remote" }, server = "http://127.0.0.1:1234";
  const terminal = { address: row.address, created_ms: 50, foreground: { pid: 44, argv: ["opencode", "--server", server] } };
  const requests = [];
  const { monitor, cx, tasks, tick } = bridgeFixture(async (_cmd, args) => {
    requests.push(JSON.parse(args[2])[0]);
    const info = { ...live, sessionID: "ses_found", sessionTitle: requests.length === 1 ? "Task" : "Renamed",
      resumeVerified: true, meta: { modelID: "model", contextTokens: 123, durationMs: 900 } };
    return { code: 0, stdout: JSON.stringify({ connected: true, snapshot: { t1: info } }) };
  }, { agents: () => [row], terminals: () => [terminal],
    spaces: () => [{ id: "remote", machine: "peer" }], machines: () => [{ name: "peer", layout: "ready" }] });
  monitor.refresh(cx);
  await tasks[0];
  assert.deepEqual(requests[0].resumeProbe, { server });
  assert.equal(requests[0].sessionID, null);
  assert.equal(monitor.boundSessionID(row, terminal, false, "peer"), null);
  const before = monitor.presentation(row);
  assert.equal(before.sessionTitle, "Task");
  assert.equal(monitor.details(row).contextTokens, 123);
  tick();
  await tasks[1];
  assert.equal(requests[1].presentationID, "ses_found");
  assert.equal(monitor.presentation(row).sessionTitle, "Renamed");
  assert.equal(monitor.details(row).durationMs, 900);
  monitor.accept({ t1: { ...failure, presentationKey: before.presentationKey } });
  assert.equal(monitor.details(row).modelID, "model", "a temporary read error must not erase statistics");
  assert.equal(monitor.presentation(row).sessionTitle, "Renamed", "temporary transport error preserves the display");
  const lastGood = monitor.good.t1;
  monitor.accept({ t1: { ...failure, presentationKey: before.presentationKey, matchMissing: true } });
  assert.equal(monitor.presentation(row), null, "the reader confirmed no unambiguous match");
  assert.equal(monitor.details(row), null);
  monitor.accept({ t1: lastGood });
  monitor.checkPresentation(row, { ...terminal, foreground: { ...terminal.foreground, pid: 45 } }, "peer");
  assert.equal(monitor.presentation(row), null, "another process cannot inherit a title-only match");
  assert.equal(monitor.details(row), null);
  monitor.close();
});

test("background sessions appear without terminal targets and disappear on completion or disconnect", async () => {
  let topology = [{ name: null, layout: "ready" }, { name: "ssh-host", layout: "ready" }];
  const item = { sessionID: "ses_same", title: "Real job", folder: "/job" };
  let running = true, calls = 0;
  const { monitor, cx, tasks, tick } = bridgeFixture(async (_cmd, args, { on, timeout }) => {
    assert.equal(timeout, 27000);
    assert.deepEqual(JSON.parse(args[2]), []);
    calls++;
    return { code: 0, stdout: JSON.stringify({ connected: true, key: "ok", snapshot: {},
      background: running || on ? [item] : [] }) };
  }, { agents: () => [], machines: () => topology });
  monitor.refresh(cx);
  await Promise.all([...tasks]);
  assert.equal(calls, 2);
  assert.equal(monitor.backgroundRows([]).length, 2, "IDs are scoped to the machine's managed service");
  const rows = monitor.backgroundRows();
  assert.equal(rows.length, 2, "the panel reconciles these against current terminal reports");
  assert.ok(rows.some(row => row.machine === "ssh-host"));
  assert.match(rows[0].address, /^opencode-session:/);
  running = false;
  topology = [topology[0], { name: "ssh-host", layout: "unavailable" }];
  monitor.refresh(cx);
  tick();
  await Promise.all([...tasks]);
  assert.equal(monitor.backgroundRows([]).length, 0);
  assert.equal(calls, 3, "do not query a disconnected machine");
  monitor.close();
});

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

test("a reply from the first A is stale after A → B → A, even when B never starts a query", async () => {
  const row = { ...agent, app: "opencode", status: "working", space: "local" };
  const terminal = { address: row.address, created_ms: 5, foreground: {
    pid: 42, argv: ["opencode", "--server", "http://127.0.0.1:1234", "--session", "ses_1"],
  } };
  const replies = [];
  const { monitor, cx, tasks, tick } = bridgeFixture(() => new Promise(resolve => replies.push(resolve)),
    { agents: () => [row], terminals: () => [terminal] });
  monitor.refresh(cx);
  await new Promise(resolve => setImmediate(resolve));
  terminal.foreground.pid = 43;
  monitor.refresh(cx);
  terminal.foreground.pid = 42;
  monitor.refresh(cx);
  assert.equal(replies.length, 1);
  const packet = key => ({ code: 0, stdout: JSON.stringify({ connected: true, key, snapshot: {
    t1: { ...live, sessionID: "ses_1", resumeVerified: true, sessionTitle: key },
  } }) });
  replies[0](packet("stale"));
  await tasks[0];
  assert.equal(monitor.boundSessionID(row, terminal), null);
  assert.equal(monitor.session(row, "ses_1"), null);
  assert.equal(monitor.bridgeKeys.has(null), false);
  tick();
  await new Promise(resolve => setImmediate(resolve));
  replies[1](packet("fresh"));
  await tasks[1];
  assert.equal(monitor.boundSessionID(row, terminal), "ses_1");
  assert.equal(monitor.session(row, "ses_1").sessionTitle, "fresh");
  monitor.close();
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
