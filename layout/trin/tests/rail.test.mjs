import { createHash } from "node:crypto";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

function element() {
  const state = { childNodes: [], attrs: {}, handlers: {} };
  const el = new Proxy(state, { get(target, key) {
    if (key in target) return target[key];
    return (...args) => {
      if (key === "child") state.childNodes.push(args[0]);
      else if (key === "children") state.childNodes.push(...args[0]);
      else if (key === "when") { if (args[0]) args[1](el); }
      else if (key.startsWith("on_")) state.handlers[key] = args.at(-1);
      else state.attrs[key] = args;
      return el;
    };
  } });
  return el;
}
const nodes = el => el && typeof el === "object" ? [el, ...el.childNodes.flatMap(nodes)] : [];
const text = el => el && typeof el === "object" ? el.childNodes.map(text).join("") : String(el ?? "");
const byID = (el, id) => nodes(el).find(n => n.attrs.id?.[0] === id);
const read = file => fs.readFileSync(new URL(`../mods/${file}`, import.meta.url), "utf8")
  .replace(/^import .*;\n/gm, "").replace("export default class", "class").replace(/^export /gm, "");

const mouse = (y, local = 4, height = 52) => ({
  position: { x: 12, y }, local_position: { x: 12, y: local }, bounds: { width: 200, height },
});
const cx = { notify() {}, stop_propagation() {} };
const click = (row, context = cx) => {
  row.handlers.on_mouse_down(mouse(10), context);
  row.handlers.on_mouse_up(mouse(10), context);
};

function fixture(storage = {}) {
  let now = 1000;
  const spaces = [{ id: "s", name: "Herdr", active: true, tabs: [{ id: "tab", named: true, name: "Named session" }] }];
  const agents = ["working", "idle", "blocked", "done"].map((status, seq) => ({
    address: `t${seq}`, pane: `p${seq}`, space: "s", tab: "tab", status, seq,
    state: "live", focused: seq === 1, program: { name: "OpenCode" },
  }));
  const commands = [], badges = [], stored = {};
  const t = { accent: "#8ab4f8", chrome: "#101112", selected: "#25272b", muted: "#999999", dim: "#777777", idle: "#777777", working: "#d9bc65", blocked: "#e09080", done: "#8cb383" };
  const context = { createHash, View: class {}, div: element, svg: path => element().asset(path),
    image: path => element().imagePath(path), h_flex: element, v_flex: element,
    Date: { now: () => now },
    formatTokens: () => "", formatActiveTime: () => "", modelLabel: () => "model", contextTooltip: () => "",
    theme: () => t, layout: () => ({ id: "sidebar", size: { px: 44 } }),
    spaces: () => spaces, agents: () => agents, panes: () => agents.map(a => ({ ...a, terminal: a.address })), terminals: () => agents,
    shared: key => stored[key], setShared: (key, value) => { stored[key] = value; },
    focused: () => ({ agent: "t1" }), setAgentOrder: order => { context.order = order; },
    setBadge: text => badges.push(text),
    window: () => ({ platform: "macos" }),
    localStorage: { getItem: key => storage[key] ?? null, setItem(key, value) { storage[key] = value; } },
    run: (name, args) => commands.push({ name, args }),
  };
  vm.runInNewContext(read("trin-agents/order.js") + "\n" + read("trin-agents/appearance.js") + "\n"
    + ["identity", "activity", "background", "opening", "main"].map(name => read(`trin-agents/${name}.js`)).join("\n")
    + "\nthis.AgentsPanel = Agents; this.readTerminalSessionID = terminalSessionID;", context);
  const panel = new context.AgentsPanel();
  Object.assign(panel, { order: JSON.parse(storage["sessions.order"] ?? storage.pins ?? "[]"), clock: { sync() {}, value: () => null },
    activity: { boundSessionID: (_agent, terminal) => context.readTerminalSessionID(terminal),
      details: () => ({ providerID: "openai", modelID: "gpt-test", variant: "medium" }), get: () => null, notice: () => null, close() {} } });
  return { panel, commands, badges, agents, context, t, spaces, storage, advance: ms => { now += ms; } };
}

test("Dock badge sums live blocked and unseen done sessions reactively across projects", () => {
  const { panel, agents, badges, context } = fixture();
  agents[3].space = "remote/other-project";
  agents[3].tab = "remote/other-tab";
  panel.render();
  assert.deepEqual(badges, ["2"]);
  panel.render();
  assert.deepEqual(badges, ["2"], "unchanged renders do not write the badge");
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  panel.render();
  assert.deepEqual(badges, ["2"], "wide presentation does not change the count");
  agents.push({ ...agents[2] });
  panel.render();
  assert.deepEqual(badges, ["2"], "duplicate panes count once by terminal address");
  agents[3].status = "idle";
  panel.render();
  assert.deepEqual(badges, ["2", "1"], "seen done results no longer count");
  agents.splice(2);
  panel.render();
  assert.deepEqual(badges, ["2", "1", null], "removed/disconnected sessions clear the badge");
  agents.push({ address: "remote/new", status: "blocked" });
  panel.updateBadge(agents);
  assert.equal(badges.at(-1), "1", "reconnected/new sessions count immediately");
  panel.close();
  assert.equal(badges.at(-1), null, "closing the panel clears its badge");
});

test("Dock badge clears on initial empty snapshot and ignores idle and working", () => {
  const { panel, badges } = fixture();
  panel.updateBadge([]);
  panel.updateBadge([]);
  assert.deepEqual(badges, [null]);
  const seen = { address: "seen", status: "idle" };
  panel.updateBadge([seen, { address: "busy", status: "working" }]);
  assert.deepEqual(badges, [null]);
  panel.updateBadge([{ address: "waiting", status: "blocked", focused: true }]);
  assert.equal(badges.at(-1), "1", "blocked counts until resolved even while focused");
});

test("session avatar uses the project machine and configured folder and falls back to a letter", () => {
  const { panel, context, spaces } = fixture();
  spaces[0].folder = "/old";
  context.setShared("trin.projects.v1", { projects: [{ id: "P", space: "s", name: "Project", machine: "server", folder: "/repo" }] });
  const key = JSON.stringify(["server", "/repo"]), icon = `data/trin-project-icons/${"a".repeat(64)}.png`;
  context.setShared("trin.project-icons.ready", { [key]: icon });
  assert.ok(nodes(byID(panel.render(), "ag-t0")).some(e => e.attrs.imagePath?.[0] === icon));
  context.setShared("trin.project-icons.ready", { [key]: "../../invalid.png" });
  const row = byID(panel.render(), "ag-t0");
  assert.ok(!nodes(row).some(e => e.attrs.imagePath));
  assert.ok(text(row).includes("P"));
});

test("wide Sessions tints only unread results blue and retains the status dot", () => {
  const { panel, agents, context, t } = fixture();
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  const root = panel.render();
  const done = byID(root, "ag-t3");
  assert.equal(done.attrs.bg[0], "#3b82f630");
  const hovered = done.attrs.hover[0](element());
  assert.equal(hovered.attrs.bg[0], "#3b82f63b");
  assert.ok(nodes(done).some(n => n.attrs.tooltip?.[0] === "Готово" && n.attrs.bg?.[0] === t.done));
  assert.equal(byID(root, "ag-t1").attrs.bg[0], t.selected);
  for (const id of ["ag-t0", "ag-t2"]) assert.equal(byID(root, id).attrs.bg, undefined);
  agents[3].status = "idle";
  assert.equal(byID(panel.render(), "ag-t3").attrs.bg, undefined, "reading the result clears the tint");
});

test("wide Sessions has a compact title, no sorting menu, and preserves default tab order", () => {
  const { panel, agents, context } = fixture();
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  context.setShared("agents.sort", "grouped");
  const rows = () => {
    const root = panel.render();
    assert.equal(byID(root, "ag-sort"), undefined);
    assert.equal(text(byID(root, "sessions-title")), "Сессии");
    assert.doesNotMatch(text(root), /Agents|Needs you|Working|Idle|Pinned/);
    return byID(root, "ag-list").childNodes.map(n => n.attrs.id[0]);
  };
  assert.deepEqual(Array.from(rows()), ["ag-t0", "ag-t1", "ag-t2", "ag-t3"]);
  agents[0].status = "done";
  agents[3].status = "working";
  assert.deepEqual(Array.from(rows()), ["ag-t0", "ag-t1", "ag-t2", "ag-t3"]);
  panel.order = ["t2"];
  assert.deepEqual(Array.from(rows()), ["ag-t2", "ag-t0", "ag-t1", "ag-t3"]);
  assert.deepEqual(Array.from(context.order), ["t2", "t0", "t1", "t3"]);
});

test("only main Herdr sessions appear, even when child terminals have panes or pins", () => {
  const { panel, agents, badges, context } = fixture();
  const child = { address: "child", pane: "child-pane", status: "blocked", state: "live" };
  const orphan = { address: "orphan", pane: "orphan-pane", status: "done", state: "live" };
  agents.push(child, orphan);
  context.terminals = () => [...agents.slice(0, 4), { ...child, parent: "t0" },
    { ...orphan, parent: "offline/starter" }, { address: "nested", parent: "child", state: "live" }];
  panel.order = ["child"];
  for (const width of [44, 268]) {
    context.layout = () => ({ id: "sidebar", size: { px: width } });
    const root = panel.render();
    for (const id of ["ag-child", "ag-orphan", "sub-child", "sub-nested", "fold-t0"]) {
      assert.equal(byID(root, id), undefined);
    }
    assert.ok(byID(root, "ag-t0"));
    assert.deepEqual(Array.from(context.order), ["t0", "t1", "t2", "t3"]);
  }
  assert.deepEqual(badges, ["2"]);
});

test("background rows can be opened, without fabricated terminal navigation entries", () => {
  const { panel, commands, agents, context, advance } = fixture();
  const opened = [];
  panel.openBackground = a => opened.push(a.sessionID);
  panel.activity.backgroundRows = () => [{ address: "opencode-session:/ses_job", sessionID: "ses_job", background: true,
    title: "Background job", folder: "/fixture", program: { name: "OpenCode" }, status: "working" }];
  panel.activity.session = a => a === agents[1] ? { excluded: true } : null;
  panel.render();
  advance(1000);
  for (const width of [44, 268]) {
    context.layout = () => ({ id: "sidebar", size: { px: width } });
    const root = panel.render();
    assert.equal(byID(root, "ag-t1"), undefined);
    const row = byID(root, "ag-opencode-session:/ses_job");
    assert.ok(row);
    click(row);
    assert.deepEqual(Array.from(context.order), ["t0", "t2", "t3"]);
    if (width === 44) assert.match(row.attrs.tooltip[0], /Background job.*\n.*\n.*\nФоновая сессия/);
    else assert.match(text(row), /Background job.*В фоне/);
  }
  assert.deepEqual(opened, ["ses_job", "ses_job"]);
  assert.deepEqual(commands, []);
});

test("clicking a background session resumes its existing ID on its owning machine, with duplicate clicks locked", async () => {
  const { panel, commands, context, spaces } = fixture();
  const calls = [], pending = [];
  spaces.push({ id: "r", name: "Remote", folder: "/fixture", machine: "server", tabs: [] });
  context.machines = () => [{ name: "server", layout: "ready" }];
  context.exec = async (_file, [, command, args]) => {
    calls.push([command, JSON.parse(args)]);
    return { code: 0, stdout: '{"space":"r","tab":"resumed"}' };
  };
  const agent = { address: "opencode-session:server/ses_job", sessionID: "ses_job", folder: "/fixture", machine: "server" };
  const asyncCtx = { ...cx, spawn: fn => pending.push(fn(asyncCtx)) };
  panel.openBackground(agent, asyncCtx);
  panel.openBackground(agent, asyncCtx);
  await Promise.all(pending);
  assert.deepEqual(calls, [["tab.new", { space: "r", folder: "/fixture", argv: ["opencode", "--session", "ses_job"], focus: false }]]);
  assert.equal(commands[0].args.id, "resumed");
});

test("a completed remote row yields to its opening tab, even if the background snapshot keeps it", async () => {
  const { panel, agents, spaces, context, commands, advance } = fixture();
  const remote = { id: "r", name: "Remote", folder: "/fixture", machine: "server", tabs: [] };
  spaces.push(remote);
  context.machines = () => [{ name: "server", layout: "ready" }];
  let acknowledge;
  const reply = new Promise(resolve => { acknowledge = resolve; });
  let launches = 0;
  context.exec = () => { launches++; return reply; };
  const jobs = [], launchCtx = { ...cx, spawn: fn => jobs.push(fn(launchCtx)) };
  const task = { address: "opencode-session:server/ses_result", sessionID: "ses_result", title: "Result",
    folder: "/fixture", machine: "server", background: true, status: "done" };
  const local = { ...task, machine: null, address: "opencode-session:/ses_result" };
  panel.activity.backgroundRows = () => [task, local];
  panel.render();
  advance(1000);
  const stale = byID(panel.render(), `ag-${task.address}`);
  assert.ok(stale);
  click(stale, launchCtx);
  panel.openBackground(task, launchCtx);
  assert.equal(launches, 1);
  acknowledge({ code: 0, stdout: '{"space":"r","tab":"remote-tab","terminal":"remote-terminal"}' });
  await Promise.all(jobs);
  remote.tabs.push({ id: "remote-tab" });
  panel.render();
  panel.openBackground(task, launchCtx);
  const opened = { ...agents[1], address: "remote-terminal", pane: "remote-pane", space: "r", tab: "remote-tab",
    app: "opencode", title: task.title };
  agents.push(opened);
  let root = panel.render();
  assert.ok(byID(root, "ag-remote-terminal"));
  assert.ok(!byID(root, `ag-${task.address}`), "acknowledged terminal suppresses the launch-gap duplicate");
  assert.ok(byID(root, `ag-${local.address}`), "same ID on another machine stays independent");
  advance(16000);
  assert.ok(!byID(panel.render(), `ag-${task.address}`), "slow confirmation does not add a duplicate");
  panel.openBackground(task, launchCtx);
  assert.equal(launches, 1, "a visible tab does not unlock when confirmation is slow");
  assert.equal(commands.at(-1).args.id, "remote-tab");
  opened.report = { app: "opencode", state: "idle", title: task.title };
  opened.records = { ses_result: opened.report };
  panel.render();
  advance(16000);
  root = panel.render();
  assert.ok(!byID(root, `ag-${task.address}`));
  click(stale, launchCtx);
  assert.equal(commands.at(-1).args.pane, "remote-pane");
  assert.equal(launches, 1);
});

test("background click reads new attachments before render, and late acknowledgement preserves project choice", async () => {
  const { panel, agents, spaces, context, commands } = fixture();
  const remote = { id: "other", machine: "peer", folder: "/other", active: true, tabs: [{ id: "open" }] };
  spaces.push(remote);
  context.machines = () => [{ name: "peer", layout: "ready" }];
  const registry = { selected: "other", projects: [{ id: "other", space: "other", machine: "peer", folder: "/other" }] };
  context.setShared("trin.projects.v1", registry);
  const task = { address: "opencode-session:peer/ses_remote", machine: "peer", sessionID: "ses_remote", folder: "/home" };
  const report = { state: "working", title: "Job", app: "opencode" };
  agents.push({ ...agents[0], address: "new", pane: "new-pane", space: "other", tab: "open", report, records: { ses_remote: report } });
  panel.openBackground(task, cx);
  assert.equal(commands.at(-1).args.pane, "new-pane");
  agents.pop();
  const jobs = [], calls = [];
  let reply;
  context.exec = (_file, [, cmd, args]) => { calls.push([cmd, JSON.parse(args)]); return new Promise(resolve => { reply = resolve; }); };
  const ctx = { ...cx, spawn: fn => jobs.push(fn(ctx)) };
  panel.openBackground(task, ctx);
  assert.equal(calls[0][0], "space.new"); assert.equal(calls[0][1].folder, "/home");
  context.setShared("trin.projects.v1", { ...registry, selected: "elsewhere" });
  reply({ code: 0, stdout: '{"space":"home","tab":"new-tab"}' }); await Promise.all(jobs);
  assert.equal(commands.length, 1);
  assert.equal(context.shared("trin.projects.v1").selected, "elsewhere");
});

test("closing the unconfirmed terminal unlocks a new launch even if a shell keeps the tab open", async () => {
  const { panel, context, spaces, agents } = fixture();
  const remote = { id: "r", name: "Remote", folder: "/fixture", machine: "server", tabs: [] };
  spaces.push(remote);
  context.machines = () => [{ name: "server", layout: "ready" }];
  let launches = 0;
  context.exec = async () => {
    launches++;
    return { code: 0, stdout: '{"space":"r","tab":"r-tab","terminal":"r-term"}' };
  };
  const jobs = [], launchCtx = { ...cx, spawn: fn => jobs.push(fn(launchCtx)) };
  const task = { address: "opencode-session:server/ses_result", sessionID: "ses_result", folder: "/fixture", machine: "server" };
  panel.openBackground(task, launchCtx);
  await Promise.all(jobs);
  remote.tabs.push({ id: "r-tab" });
  agents.push({ ...agents[1], address: "r-shell", space: "r", tab: "r-tab", app: "shell" });
  agents.push({ ...agents[1], address: "r-term", space: "r", tab: "r-tab", app: "opencode" });
  panel.render();
  agents.pop();
  panel.render();
  panel.openBackground(task, launchCtx);
  await Promise.all(jobs);
  assert.equal(launches, 2);
});

for (const program of [{ shell: true }, { key: "codex", agent: true }]) {
  test(`opening lock survives independent pane/terminal gaps, releases on verified ${program.key ?? "shell"} replacement`, () => {
    const { context } = fixture();
    const opens = vm.runInNewContext("new BackgroundOpens()", context), ticket = opens.begin("session");
    opens.acknowledge(ticket, { tab: "tab", terminal: "term" }, 0);
    const original = { foreground: { pid: 71 }, program: { key: "opencode", agent: true } };
    const live = new Map([["term", original]]);
    const state = (present, owner, now, pane = true) => opens.reconcile(new Map(present ? [["tab", {}]] : []), new Map(),
      new Map(owner ? [["tab", [{ address: "term" }]]] : []), live, now, pane ? [{ tab: "tab", pane: "p", terminal: "term" }] : []);
    assert.ok(state(true, true, 1).claimed.has("session"));
    assert.ok(state(false, false, 17000, false).claimed.has("session")); assert.equal(opens.begin("session"), null);
    live.delete("term");
    assert.ok(state(true, false, 17500).claimed.has("session")); assert.equal(opens.begin("session"), null);
    live.set("term", original); assert.ok(state(true, true, 18000).claimed.has("session"));
    live.set("term", { foreground: { pid: 72 }, program: { key: "program" } });
    assert.ok(state(true, false, 18500).claimed.has("session"), "new PID alone is not a different app");
    live.set("term", { foreground: { pid: 72 }, program });
    assert.ok(!state(true, false, 19000).claimed.has("session")); assert.ok(opens.begin("session"));
  });
}

test("a created tab without its acknowledged terminal unlocks after the startup deadline", async () => {
  const { panel, spaces, agents, context, advance } = fixture();
  const remote = { id: "r", name: "Remote", folder: "/fixture", machine: "server", tabs: [] };
  spaces.push(remote);
  context.machines = () => [{ name: "server", layout: "ready" }];
  let launches = 0;
  context.exec = async () => {
    launches++;
    return { code: 0, stdout: '{"space":"r","tab":"r-tab","terminal":"never-appears"}' };
  };
  const jobs = [];
  let wake;
  const launchCtx = { ...cx, spawn: fn => jobs.push(fn(launchCtx)), timer: { after(ms, fn) {
    wake = { ms, fn, cancel() {} };
    return wake;
  } } };
  const task = { address: "opencode-session:server/ses_result", sessionID: "ses_result", folder: "/fixture", machine: "server" };
  panel.openBackground(task, launchCtx);
  await Promise.all(jobs);
  remote.tabs.push({ id: "r-tab" });
  agents.push({ ...agents[1], address: "r-shell", space: "r", tab: "r-tab", app: "shell" });
  panel.live = false;
  panel.render(launchCtx);
  assert.equal(wake.ms, 15000);
  panel.openBackground(task, launchCtx);
  assert.equal(launches, 1);
  advance(wake.ms);
  wake.fn(launchCtx);
  panel.render(launchCtx);
  panel.openBackground(task, launchCtx);
  await Promise.all(jobs);
  assert.equal(launches, 2);
});

test("working hidden tabs live below the folded section and return to the main list on a result", () => {
  const { panel, commands, agents, spaces } = fixture();
  spaces[0].tabs.push({ id: "hidden", name: "Hidden job", named: true, settled: true });
  const job = agents[0];
  job.tab = "hidden";
  let root = panel.render();
  assert.equal(byID(root, "ag-t0"), undefined);
  const toggle = byID(root, "hidden-sessions-toggle");
  assert.ok(toggle);
  toggle.handlers.on_click({}, cx);
  root = panel.render();
  const hidden = byID(byID(root, "hidden-sessions"), "ag-t0");
  assert.ok(hidden);
  hidden.handlers.on_click({}, cx);
  assert.equal(commands.at(-1).args.pane, "p0");
  job.status = "done";
  root = panel.render();
  assert.ok(byID(root, "ag-t0"));
  assert.equal(byID(root, "hidden-sessions"), undefined);
});

test("Hidden appears below the scrolling list only when populated, with an edge-to-edge divider", () => {
  const { panel, context, agents, spaces } = fixture();
  const tab = { id: "hidden", settled: false };
  spaces[0].tabs.push(tab);
  agents[0].tab = tab.id;
  for (const width of [44, 268]) {
    context.layout = () => ({ id: "sidebar", size: { px: width } });
    tab.settled = false;
    assert.equal(byID(panel.render(), "hidden-sessions"), undefined);
    tab.settled = true;
    const root = panel.render();
    const section = byID(root, "hidden-sessions");
    assert.equal(root.childNodes[root.childNodes.indexOf(byID(root, "ag-list")) + 1], section);
    assert.equal(section.attrs.px, undefined, "section does not inset the full-width divider");
    const toggle = byID(section, "hidden-sessions-toggle");
    assert.ok(toggle.attrs.w_full);
    assert.equal(toggle.attrs.border_t[0], 1);
    assert.equal(toggle.attrs.px[0], width === 44 ? 2 : 14);
    assert.match(toggle.attrs.tooltip[0], /: 1$/);
    assert.equal(section.attrs.h[0], 30);
    toggle.handlers.on_click({}, cx);
    const opened = byID(panel.render(), "hidden-sessions");
    const list = byID(opened, "hidden-sessions-list");
    const row = byID(list, "ag-t0");
    assert.equal(opened.attrs.h[0], 30 + row.attrs.h[0]);
    assert.equal(row.attrs.h[0], width === 44 ? 41 : 57);
    assert.equal(row.attrs.pb[0], width === 44 ? 5 : 11);
    assert.ok(list.attrs.flex_1 && list.attrs.overflow_y_scrollbar);
    byID(opened, "hidden-sessions-toggle").handlers.on_click({}, cx);
  }
});

test("the expanded section caps its viewport without dropping any hidden row", () => {
  const { panel, spaces, context } = fixture();
  panel.hiddenOpen = true;
  const ps = [];
  context.panes = () => ps;
  for (let n = 1; n <= 6; n++) {
    spaces[0].tabs.push({ id: `hidden${n}`, settled: true, panes: 1 });
    ps.push({ tab: `hidden${n}`, pane: `mod${n}`, mod: "fixture" });
  }
  const section = byID(panel.render(), "hidden-sessions");
  const list = byID(section, "hidden-sessions-list");
  assert.equal(section.attrs.h[0], 30 + 3 * 41);
  assert.equal(list.childNodes.length, 6);
});

test("the Details tooltip distinguishes history model from the selected model", () => {
  const { panel, context } = fixture();
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  panel.activity.details = () => ({ providerID: "opencode-go", modelID: "example-free", modelFromHistory: true });
  const expected = /opencode-go\/example-free\nМодель последнего шага/;
  assert.match(byID(panel.render(), "agent-t0-model").attrs.tooltip[0], expected);
  context.layout = () => ({ id: "sidebar", size: { px: 44 } });
  assert.match(byID(panel.render(), "ag-t0").attrs.tooltip[0], expected);
});
const sessionRows = panel => Array.from(byID(panel.render(), "ag-list").childNodes, row => row.attrs.id[0].slice(3));

test("sessions support drop before and after, persist order and preserve click-to-focus in both widths", () => {
  for (const [width, height] of [[268, 52], [44, 36]]) {
    const { panel, commands, context, storage } = fixture({ pins: '["t1"]' });
    context.layout = () => ({ id: "sidebar", size: { px: width } });
    assert.deepEqual(sessionRows(panel), ["t1", "t0", "t2", "t3"]);
    const row = id => byID(panel.render(), `ag-${id}`);
    const drop = (from, to, after) => {
      row(from).handlers.on_mouse_down(mouse(10), cx);
      const event = mouse(70, after ? height - 4 : 4, height);
      row(to).handlers.on_mouse_move(event, cx);
      assert.equal(byID(row(to), `session-drop-${to}`).attrs[after ? "bottom" : "top"][0], 0);
      assert.equal(row(from).attrs.opacity[0], 0.5);
      row(to).handlers.on_mouse_up(event, cx);
      assert.equal(panel.drag, null);
      assert.equal(byID(row(to), `session-drop-${to}`), undefined);
    };
    drop("t1", "t3", true);
    assert.deepEqual(sessionRows(panel), ["t0", "t2", "t3", "t1"]);
    drop("t1", "t2", false);
    assert.deepEqual(sessionRows(panel), ["t0", "t1", "t2", "t3"]);
    assert.deepEqual(sessionRows(fixture(storage).panel), ["t0", "t1", "t2", "t3"]);
    assert.deepEqual(commands, [], "dragging never changes the focused pane");
    click(row("t1"));
    assert.equal(commands.at(-1).args.pane, "p1");
  }
});

test("a background session can be reordered and keeps its position when it gets a pane", () => {
  const { panel, agents, storage, commands, advance } = fixture();
  const detached = { address: "opencode-session:/ses_job", sessionID: "ses_job", background: true,
    title: "Background job", folder: "/fixture", program: { name: "OpenCode" }, status: "working" };
  let running = true;
  panel.activity.backgroundRows = () => running ? [detached] : [];
  panel.render();
  advance(1000);
  const row = id => byID(panel.render(), `ag-${id}`);
  row(detached.address).handlers.on_mouse_down(mouse(10), cx);
  row("t0").handlers.on_mouse_move(mouse(70), cx);
  row("t0").handlers.on_mouse_up(mouse(70), cx);
  assert.equal(sessionRows(panel)[0], detached.address);
  assert.equal(JSON.parse(storage["sessions.order"])[0], detached.address);
  running = false;
  assert.equal(sessionRows(panel)[0], "t0");
  const report = { app: "opencode", state: "working", title: "Title from the report" };
  const attached = { ...agents[0], address: "t-resumed", report, records: { ses_job: report } };
  agents.push(attached);
  assert.equal(sessionRows(panel)[0], attached.address);
  attached.title = "Renamed";
  assert.equal(sessionRows(panel)[0], attached.address, "transient rename gap does not change manual rank");
  assert.deepEqual(commands, []);
});

test("live terminal IDs merge immediately, even with different titles; different IDs do not merge", () => {
  const { panel, agents, badges, context, advance } = fixture();
  const report = { app: "opencode", state: "done", title: "Full title before rename" };
  Object.assign(agents[3], { report, records: { ses_job: report }, title: "Truncated terminal title…" });
  const secondPane = { ...agents[3], address: "t-same", focused: true };
  agents.push(secondPane);
  const job = { address: "opencode-session:/ses_job", background: true, title: "New title", status: "working" };
  const unrelated = { ...job, address: "opencode-session:/ses_other", title: agents[3].title };
  const remote = { ...job, address: "opencode-session:server/ses_job", machine: "server" };
  panel.activity.backgroundRows = () => [job, unrelated, remote];
  panel.activity.session = () => null; // no results from the async bridge yet
  panel.render();
  advance(1000);
  assert.deepEqual(sessionRows(panel), ["t0", "t1", "t2", "t-same", unrelated.address, remote.address]);
  assert.deepEqual(Array.from(context.order), ["t0", "t1", "t2", "t-same"]);
  assert.deepEqual(badges, ["2"], "the same completed session counts only once");
  agents.pop();
  agents[3].records = { ses_next: report };
  panel.render();
  advance(1000);
  assert.ok(sessionRows(panel).includes(job.address), "switching IDs releases the previous session");
});

test("project selection, settle and restoration use the home client, never the duplicate's native caption", () => {
  const { panel, spaces, agents, context, commands } = fixture();
  const project = (id, tabs) => ({ id, machine: "remote", folder: "/" + id, tabs });
  const tab = (id, name, active) => ({ id, name, named: true, active, panes: 1, status: "working" });
  const home = project("home", [tab("a", "Old native home caption", true)]);
  const other = project("other", [tab("c", "Tools", false), tab("b", "Tools", true), tab("d", "Third", false)]);
  spaces.splice(0, spaces.length, home, other);
  const registry = { selected: "other", projects: spaces.map(s => ({ ...s, space: s.id })) };
  context.setShared("trin.projects.v1", registry);
  other.active = true;
  const client = (n, space, tab, id, title) => {
    const report = { app: "opencode", state: "working", title };
    return { address: "t" + n, pane: "p" + n, pid: n, created_ms: n, space, tab, title, report,
      program: { key: "opencode", name: "OpenCode", agent: true }, records: { [id]: report }, status: "working" };
  };
  const a = client(1, "home", "a", "ses_portal", "Portal");
  const b = client(2, "other", "b", "ses_portal", "Portal");
  const c = client(3, "other", "c", "ses_tools", "Tools");
  b.focused = true;
  agents.splice(0, agents.length, b, c, a);
  const ts = [a, b, c]; let missingPane, missingTerminal;
  const extra = [];
  context.terminals = () => ts.filter(t => t !== missingTerminal);
  context.machines = () => []; // Native snapshots only; no background service calls.
  context.panes = () => [...ts.filter(t => t.pane !== missingPane).map(t => ({ ...t, terminal: t.address })), ...extra];
  // Apply the actual commands emitted by both views to native state.
  const select = (s, t) => {
    spaces.forEach(x => { x.active = x === s; }); s.tabs.forEach(x => { x.active = x === t; }); t.settled = false;
  };
  context.run = (name, args) => {
    commands.push({ name, args });
    for (const s of spaces) for (const t of s.tabs) {
      if (name === "tab.select" && args.id === t.id) select(s, t);
      if (name === "tab.settle" && args.id === t.id) t.settled = true;
      if (name === "pane.focus" && context.panes().some(p => p.pane === args.pane && p.tab === t.id)) select(s, t);
    }
  };
  const monitor = vm.runInNewContext("new ActivityMonitor()", context);
  panel.activity = monitor;
  panel.hiddenOpen = panel.live = true;
  monitor.session = (t, id) => id === "ses_portal" && t.address === b.address ? { sessionFolder: "/home", sessionTitle: "Portal" }
    : id === "ses_tools" ? { sessionFolder: "/other", sessionTitle: "Tools" } : null;
  const meta = { providerID: "fixture", modelID: "portal-model", durationMs: 12345, durationReady: true };
  monitor.details = (t, id) => t.address === b.address && id === "ses_portal" ? meta : null;
  panel.clock.sync = (roots, details) => {
    if (!missingTerminal && roots.some(t => t.address === a.address) && monitor.bindings.get(a.address)?.id === "ses_portal"
      && monitor.bindings.get(b.address)?.id === "ses_portal" && !Object.keys(b.records).includes("ses_tools")) {
      assert.equal(details(a)?.durationMs, 12345);
    }
  };
  const tabsContext = { ...context, props: () => ({}), settings: () => ({ zoom: 1 }) };
  vm.runInNewContext(read("trin-tabs/projection.js") + "\n" + read("trin-tabs/main.js") + "\nthis.TabView = Tabs;", tabsContext);
  const strip = new tabsContext.TabView(); strip.panels = {};
  let root = panel.render();
  assert.ok(byID(root, "ag-t1")); assert.ok(byID(root, "ag-t3")); assert.ok(!byID(root, "ag-t2"));
  const bar = strip.render();
  assert.ok(!byID(bar, "tab-b")); assert.ok(byID(bar, "tab-c"));
  const active = () => spaces.find(s => s.active).tabs.find(t => t.active)?.id;
  assert.equal(active(), "c");
  strip.navigate("tab-next"); assert.equal(active(), "d"); strip.navigate("tab-prev"); assert.equal(active(), "c");
  strip.navigate("tab-prev"); assert.equal(active(), "d"); strip.navigate("tab-next"); assert.equal(active(), "c");
  strip.navigate("tab-index--2"); assert.equal(active(), "d"); strip.navigate("tab-index--3"); assert.equal(active(), "d");
  byID(strip.render(), "tabs-root").handlers.on_scroll_wheel({ delta: { y: 2 } }); assert.equal(active(), "c");
  // Actual split snapshot, not just a claimed pane count.
  extra.push({ tab: "b", pane: "shell-pane", terminal: "shell", program: { shell: true } });
  other.tabs[1].panes = 2; assert.ok(byID(strip.render(), "tab-b")); extra.pop(); other.tabs[1].panes = 1;
  a.pid++; assert.ok(byID(strip.render(), "tab-b")); a.pid--;
  // The remote client restarts under the default service. The old native
  // report survives the process switch; repaint BOTH views before asserting.
  missingTerminal = b;
  root = panel.render(cx); // Runs refresh/prune with the exact pane and agent still present.
  assert.ok(!byID(root, "ag-t2")); assert.equal(context.shared("trin.sessions.titles").t2, undefined);
  assert.ok(byID(strip.render(), "tab-b"));
  missingTerminal = null;
  b.foreground = { pid: b.pid + 100, argv: ["opencode", "--auto"] };
  assert.ok(byID(strip.render(), "tab-b"));
  root = panel.render(cx); assert.ok(byID(root, "ag-t2")); assert.ok(byID(strip.render(), "tab-b"));
  root = panel.render(cx); assert.ok(byID(root, "ag-t2")); assert.ok(byID(strip.render(), "tab-b"));
  b.report = { ...b.report, title: "Updated Portal report" }; b.records = { ses_portal: b.report };
  panel.render(); assert.ok(!byID(strip.render(), "tab-b"), "fresh report confirms new process");
  const count = commands.length; strip.closeOrHide("b"); assert.equal(commands.length, count);
  // The home project's real × must settle the home tab only. Restore from
  // the other project while agents() is incomplete, then render both views.
  const chooseProject = s => { context.setShared("trin.projects.v1", { ...registry, selected: s.id }); spaces.forEach(x => { x.active = x === s; }); };
  chooseProject(home);
  byID(strip.render(), "close-a").handlers.on_click({}, cx);
  assert.equal(commands.at(-1).name, "tab.settle"); assert.equal(commands.at(-1).args.id, "a");
  assert.ok(home.tabs[0].settled); assert.equal(other.tabs[1].settled, undefined);
  chooseProject(other); agents.splice(0, agents.length, c);
  root = panel.render();
  const hidden = byID(root, "hidden-sessions-list"); assert.equal(hidden.childNodes.length, 1);
  const restore = byID(hidden, "ag-t1"); assert.match(restore.attrs.tooltip[0], /Portal/); assert.match(restore.attrs.tooltip[0], /portal-model/);
  agents.push(a); missingPane = a.pane;
  const before = commands.length; restore.handlers.on_click({}, cx); assert.equal(commands.length, before);
  assert.equal(context.shared("trin.projects.v1").selected, "other"); missingPane = null;
  restore.handlers.on_click({}, cx); assert.equal(context.shared("trin.projects.v1").selected, "home");
  assert.equal(commands.at(-1).args.pane, "p1"); assert.equal(active(), "a");
  assert.match(text(byID(strip.render(), "tab-a")), /Portal/); assert.ok(!byID(panel.render(), "hidden-sessions"));
  chooseProject(other); assert.ok(byID(strip.render(), "tab-c"));
  // Both previously bound clients idle, no records and no launch ID.
  for (const t of [a, b]) { t.records = {}; t.report = { ...t.report, state: "idle" }; t.status = "idle"; }
  panel.render(); assert.ok(!byID(strip.render(), "tab-b"));
  b.status = "working"; assert.ok(byID(strip.render(), "tab-b"), "working retained caption never hides a tab");
  b.status = "idle"; b.records = { ses_tools: { ...b.report, title: "different native record" } };
  assert.ok(byID(strip.render(), "tab-b"), "contradictory record invalidates old association even before publish");
  root = panel.render(); assert.ok(byID(root, "ag-t2")); assert.ok(!context.shared("trin.sessions.titles").t2?.mirrorOf);
  assert.ok(byID(root, "ag-t3"), "genuine Tools is independent");

});

test("first-message race: a terminal that reports its ID within the ownership grace has no second row", () => {
  const { panel, agents, advance } = fixture();
  const row = agents[0];
  row.app = "opencode";
  row.title = "OpenCode";
  const incoming = [
    { address: "opencode-session:/ses_new", title: "First prompt title", status: "working", background: true },
    { address: "opencode-session:server/ses_remote", title: "Other machine", status: "working", background: true, machine: "server" },
  ];
  panel.activity.backgroundRows = () => incoming;
  const provisional = ["t0", "t1", "t2", "t3"];
  assert.deepEqual(sessionRows(panel), provisional, "a new candidate does not appear in the first frame");
  advance(3000);
  assert.deepEqual(sessionRows(panel), [...provisional, incoming[1].address],
    "no duplicate even if the working terminal is still missing its session ID after the grace interval");
  row.title = "Renamed terminal";
  row.report = { app: "opencode", state: "working", title: "First prompt title" };
  row.records = { ses_new: row.report };
  assert.deepEqual(sessionRows(panel), [...provisional, incoming[1].address],
    "the identified tab owns the row, even before the API statistics arrive");
  row.status = "done";
  row.report.state = "idle";
  assert.deepEqual(sessionRows(panel), [...provisional, incoming[1].address]);
});

test("an unrelated unresolved tab cannot hide a background row beyond the bounded grace", () => {
  const { panel, agents, advance } = fixture();
  const ids = ["opencode-session:/ses_shown", "opencode-session:/ses_waiting"];
  let visible = [ids[0]];
  panel.activity.backgroundRows = () => visible.map(address => ({ address, status: "working", background: true }));
  panel.render();
  advance(1000);
  assert.ok(sessionRows(panel).includes(ids[0]));
  const row = agents[0];
  row.app = "opencode";
  row.status = "working";
  visible = ids;
  panel.render();
  advance(3000);
  assert.ok(sessionRows(panel).includes(ids[0]), "existing rows do not flicker");
  assert.ok(!sessionRows(panel).includes(ids[1]));
  advance(1000);
  assert.deepEqual(sessionRows(panel).slice(-2), ids);
});

test("a previously identified session reappears after its last tab closes, despite unrelated ambiguity", () => {
  const { panel, agents, advance } = fixture();
  const report = { app: "opencode", state: "working", title: "Known" };
  const owned = Object.assign(agents[0], { app: "opencode", report, records: { ses_known: report } });
  const other = Object.assign(agents[1], { app: "opencode", status: "working" });
  const detached = { address: "opencode-session:/ses_known", status: "working", background: true };
  const duplicate = { address: "opencode-session:/ses_other", status: "working", background: true };
  panel.render();
  // An omitted idle report isn't proof the terminal went away.
  owned.status = "idle";
  owned.records = {};
  panel.activity.backgroundRows = () => [detached, duplicate];
  panel.render();
  advance(1000);
  assert.ok(!sessionRows(panel).includes(detached.address));
  // Closing the tab removes its terminal, unlike losing an idle report.
  agents.shift();
  panel.render();
  advance(1000);
  assert.ok(sessionRows(panel).includes(detached.address));
  assert.ok(!sessionRows(panel).includes(duplicate.address));
  // A newly confirmed terminal takes precedence again.
  Object.assign(other, { report, records: { ses_known: report } });
  assert.ok(!sessionRows(panel).includes(detached.address));
});

test("an unseen external result stays clickable while an already opened result does not come back after close", () => {
  const { panel, agents, advance } = fixture();
  const opened = [];
  panel.openBackground = a => opened.push(a.sessionID);
  const task = { address: "opencode-session:/ses_done", sessionID: "ses_done", title: "External result",
    status: "working", background: true };
  panel.activity.backgroundRows = () => [task];
  panel.render();
  task.status = "done";
  advance(1000);
  click(byID(panel.render(), `ag-${task.address}`));
  assert.deepEqual(opened, [task.sessionID]);
  const report = { app: "opencode", state: "idle", title: "External result" };
  agents.push({ ...agents[1], address: "t-resumed", report, records: { ses_done: report } });
  panel.render();
  agents.pop();
  advance(1000);
  assert.equal(byID(panel.render(), `ag-${task.address}`), undefined);
});

test("a one-shot redraw releases genuine background rows in the narrow panel without other events", () => {
  const { panel, advance } = fixture();
  const id = "opencode-session:/ses_detached";
  panel.activity.backgroundRows = () => [{ address: id, status: "working", background: true }];
  const scheduled = [];
  let redraws = 0;
  const native = { notify: () => redraws++, timer: { after(ms, fn) {
    const task = { ms, fn, cancelled: false, cancel() { this.cancelled = true; } };
    scheduled.push(task);
    return task;
  } } };
  assert.equal(byID(panel.render(native), `ag-${id}`), undefined);
  assert.equal(scheduled[0].ms, 1000);
  panel.render(native);
  assert.equal(scheduled.length, 1);
  advance(1000);
  scheduled[0].fn(native);
  assert.equal(redraws, 1);
  assert.ok(byID(panel.render(native), `ag-${id}`));
  panel.activity.backgroundRows = () => [{ address: "opencode-session:/ses_new", status: "working", background: true }];
  panel.render(native);
  panel.close();
  assert.equal(scheduled.at(-1).cancelled, true);
});

test("a display-only title does not claim a background row or become an identity", () => {
  const { panel, agents, context, advance } = fixture();
  agents[0].title = "Old terminal caption";
  agents[0].pid = 42;
  agents[0].created_ms = 100;
  panel.activity.presentation = agent => agent.address === "t0" ? { sessionTitle: "Renamed", sessionID: "ses_display" } : null;
  panel.activity.backgroundRows = () => [{ address: "opencode-session:/ses_display", sessionID: "ses_display",
    status: "done", background: true }];
  panel.live = true;
  panel.render();
  advance(1000);
  assert.ok(byID(panel.render(), "ag-opencode-session:/ses_display"));
  const info = context.shared("trin.sessions.titles").t0;
  assert.equal(info.title, "Renamed");
  assert.equal(info.presentationOnly, true);
});

test("the session list shares the current OpenCode title by terminal and session ID", () => {
  const { panel, agents, context, spaces } = fixture();
  panel.live = true;
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  spaces[0].tabs[0].named = false;
  const report = { app: "opencode", state: "working", title: "Terminal title…" };
  Object.assign(agents[0], { title: "Terminal title…", report, records: { ses_main: report } });
  let sessionTitle = "Full OpenCode session title";
  panel.activity.session = (_, id) => id === "ses_main" ? { sessionTitle } : null;
  const label = () => text(byID(panel.render(), "ag-t0"));
  assert.match(label(), /Full OpenCode session title/);
  assert.equal(context.shared("trin.sessions.titles").t0.sessionID, "ses_main");
  sessionTitle = "Renamed in OpenCode";
  assert.match(label(), /Renamed in OpenCode/);
  spaces[0].tabs[0].named = true;
  assert.match(label(), /Renamed in OpenCode/);
  agents[0].records = { ses_new: report };
  assert.match(label(), /Named session/);
  assert.equal(context.shared("trin.sessions.titles").t0, undefined);
});

test("releasing outside or losing a row during drag never focuses or saves", () => {
  for (const reason of ["outside", "gap", "missing-source", "missing-target"]) {
    const { panel, agents, storage, commands } = fixture();
    const row = id => byID(panel.render(), `ag-${id}`);
    row("t0").handlers.on_mouse_down(mouse(10), cx);
    row("t1").handlers.on_mouse_move(mouse(70), cx);
    const release = row("t1").handlers.on_mouse_up;
    const list = byID(panel.render(), "ag-list");
    if (reason === "outside") list.handlers.on_hover(false, cx);
    if (reason === "gap") list.handlers.on_mouse_up(mouse(70), cx);
    if (reason.startsWith("missing")) {
      agents.splice(reason === "missing-source" ? 0 : 1, 1);
      panel.render();
    }
    release(mouse(70), cx);
    assert.equal(panel.drag, null, reason);
    assert.equal(storage["sessions.order"], undefined, reason);
    assert.deepEqual(commands, []);
  }
});

test("agent rail uses project squares, state indicators and full session tooltips, not numbers", () => {
  const { panel, agents, commands, context } = fixture();
  const root = panel.render();
  for (const a of agents) {
    const row = byID(root, `ag-${a.address}`);
    assert.equal(row.attrs.h[0], 36);
    assert.ok(text(row).startsWith("H"));
    assert.doesNotMatch(text(row), /[0-9]/);
    assert.match(row.attrs.tooltip[0], /Named session\nHerdr\nOpenCode/);
    assert.ok(row.attrs.tooltip[0].includes(a.status));
    assert.match(row.attrs.tooltip[0], /openai\/gpt-test · medium/);
    assert.ok(nodes(row).some(n => n.attrs.size?.[0] === 22));
    click(row);
    assert.equal(commands.at(-1).args.pane, a.pane);
  }
  assert.equal(context.order.length, 4);
  const working = byID(root, "ag-t0"), idle = byID(root, "ag-t1");
  assert.ok(nodes(working).some(n => n.attrs.spin?.[0] === 900));
  assert.ok(nodes(idle).some(n => n.attrs.border_color?.[0] === "#777777"));
  assert.ok(nodes(idle).some(n => n.attrs.border_color?.[0] === "#8ab4f8"));
  for (const [address, color, message] of [["t2", "#e09080", "Нужно ваше действие"], ["t3", "#8cb383", "Готово"]]) {
    const row = byID(root, `ag-${address}`);
    assert.ok(nodes(row).some(n => n.attrs.border?.[0] === 2 && n.attrs.border_color?.[0] === color));
    assert.ok(nodes(row).some(n => n.attrs.bg?.[0] === color + "26"));
    assert.ok(nodes(row).some(n => n.attrs.bg?.[0] === (address === "t3" ? "#3b82f6" : color)));
    assert.ok(row.attrs.tooltip[0].includes(message));
  }
  assert.ok(text(byID(root, "ag-t2")).includes("!"));
  assert.ok(nodes(byID(root, "ag-t3")).some(n => n.attrs.size?.[0] === 12 && n.attrs.text_color?.[0] === "#101112"));
});

test("expanding the sidebar from the Agents rail preserves the quota height", () => {
  const { panel, commands } = fixture();
  byID(panel.render(), "collapse").handlers.on_click();
  assert.deepEqual(commands.map(({ name, args }) => [name, args.id, args.px]), [
    ["layout.resize", "sidebar", 268],
  ]);
});

test("Quotas still shows the existing provider marks", () => {
  const quotaContext = { div: element, svg: path => element().asset(path), h_flex: element, View: class {} };
  vm.runInNewContext(read("trin-quotas/main.js") + "\nthis.QuotasPanel = Quotas;", quotaContext);
  const quotaPanel = new quotaContext.QuotasPanel();
  for (const provider of ["openai", "anthropic", "ollama", "kimi", "opencode", "google"]) {
    const path = provider === "google" ? "mods/trin-agents/model-google.svg"
      : `assets/trin-providers/${provider === "opencode" ? "opencode-logo" : provider}.svg`;
    const quotaIcon = nodes(quotaPanel.mark({ provider, name: provider }, { muted: "muted" })).find(n => n.attrs.asset);
    assert.equal(quotaIcon?.attrs.asset[0], path, provider);
    assert.equal(quotaIcon.attrs.text_color[0], "muted");
  }
});

test("Sessions reserves the former icon column without rendering a provider mark or initial", () => {
  const { panel, context } = fixture();
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  context.modelLabel = id => id ?? "…";
  for (const [providerID, modelID] of [
    ["openrouter", "openai/gpt-5.5"],
    ["openai", "Opus 5.5"],
    ["opencode-go", "unclassified-model"],
    ["custom", "unclassified-model"],
    ["custom", undefined],
    [undefined, "gemini-3.1-pro"],
  ]) {
    panel.activity.details = () => ({ providerID, modelID, variant: "medium" });
    const row = byID(panel.render(), "agent-t0-model");
    const [spacer, content] = row.childNodes;
    assert.equal(row.childNodes.length, 2);
    assert.equal(row.attrs.gap[0], 7);
    assert.equal(spacer.attrs.w[0], 18);
    assert.equal(spacer.attrs.h[0], 18);
    assert.ok(spacer.attrs.flex_none);
    assert.deepEqual(spacer.childNodes, []);
    assert.ok(!nodes(row).some(n => n.attrs.asset));
    assert.equal(text(content), `${modelID ?? "…"}@medium`);
    assert.equal(row.attrs.tooltip[0], modelID ? `${providerID ?? "?"}/${modelID} · medium` : "Model data unavailable");
  }
});

test("spaces rail shows all project avatars even for folded machines and selects by ID", () => {
  const { context, t, commands, spaces } = fixture();
  const sandbox = { ...context };
  vm.runInNewContext(read("trin-spaces/main.js") + "\nthis.Panel = Spaces;", sandbox);
  const panel = new sandbox.Panel();
  panel.ordered = () => spaces.map((w, i) => ({ w, i }));
  panel.folded = new Set([null]);
  const [row] = panel.grouped(t, true);
  assert.equal(text(row), "H");
  assert.match(row.attrs.tooltip[0], /Herdr/);
  assert.ok(nodes(row).some(n => n.attrs.border_color?.[0] === t.accent));
  row.handlers.on_click();
  assert.equal(commands.at(-1).name, "space.select");
  assert.equal(commands.at(-1).args.id, "s");
});
