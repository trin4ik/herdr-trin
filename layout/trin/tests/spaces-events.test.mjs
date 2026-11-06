import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

function element() {
  let el;
  el = new Proxy({}, { get: (_target, key) => (...args) => {
    if (key === "when" && args[0]) args[1](el);
    return el;
  } });
  return el;
}
function fixture(extra = {}) {
  let now = 0, sp = [], ps = [], ms = [], notifications = 0;
  const tasks = [], every = [], after = [], requests = [];
  const stored = {};
  const context = { View: class {}, div: element, svg: element, v_flex: element, h_flex: element,
    Date: { now: () => now }, spaces: () => sp, panes: () => ps, machines: () => ms, agents: () => [],
    window: () => ({ active: true }), layout: () => ({ id: "sidebar", size: { px: 268 } }),
    theme: () => ({}), shared: key => stored[key], setShared: (key, value) => { stored[key] = value; },
    localStorage: { getItem: () => null, setItem() {} },
    hc: async () => { requests.push("machines"); return { code: 0, stdout: JSON.stringify({ machines: ms.filter(m => m.name).map(m => ({ ...m, state: "connected" })) }) }; },
    exec: async (_command, args, options) => {
      requests.push({ args, options });
      if (args.includes("status")) return { code: 0, stdout: "# branch.head main\n" };
      return { code: 0, stdout: args.includes("--git-common-dir") ? "/project/.git\n" : "/project\n" };
    },
    watchFolder: () => new Promise(() => {}),
    ...extra,
  };
  const cx = { spawn: fn => tasks.push(fn(cx)), notify: () => notifications++,
    timer: { every: (ms, callback) => every.push({ ms, callback }), after: (ms, callback) => after.push({ ms, callback }) } };
  const source = fs.readFileSync(new URL("../mods/trin-spaces/main.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace("export default class", "class");
  vm.runInNewContext(source + "\nthis.Panel = Spaces;", context);
  const panel = new context.Panel();
  return { panel, cx, tasks, every, after, requests,
    setSpaces: value => { sp = value; }, setPanes: value => { ps = value; }, setMachines: value => { ms = value; },
    advance: ms => { now += ms; }, notifications: () => notifications };
}
const space = { id: "s", name: "Project", machine: "remote", folder: "/project", tabs: [{ id: "t" }] };

test("Spaces has one slow safety timer, not 250ms data or 15s machine polling", async () => {
  const f = fixture();
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  assert.deepEqual(f.every.map(t => t.ms), [60000]);
  assert.equal(f.requests.filter(r => r === "machines").length, 1);
  f.panel.render(f.cx);
  f.panel.render(f.cx);
  assert.equal(f.requests.length, 1);
});

test("new spaces trigger Git from render immediately with one-shot identity grace", async () => {
  const f = fixture();
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  f.setSpaces([space]);
  f.panel.render(f.cx);
  f.panel.render(f.cx);
  assert.equal(f.after.length, 1);
  assert.equal(f.after[0].ms, 1500);
  await Promise.all(f.tasks);
  assert.equal(f.requests.filter(r => r?.args?.includes("status")).length, 1);
  assert.equal(f.panel.ordered().length, 1);
});

test("cwd and completion events trigger reads without waiting for the fallback", async () => {
  const f = fixture();
  f.setSpaces([space]);
  f.setPanes([{ pane: "p", tab: "t", terminal: "remote/t1", cwd: "/project", status: "working", program: { agent: true } }]);
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  f.setPanes([{ pane: "p", tab: "t", terminal: "remote/t1", cwd: "/project", status: "idle", program: { agent: true } }]);
  f.panel.render(f.cx);
  await Promise.all(f.tasks);
  assert.equal(f.requests.filter(r => r?.args?.includes("status")).length, 2);
  f.setPanes([{ pane: "p", tab: "t", terminal: "remote/t1", cwd: "/other", status: "idle", program: { agent: true } }]);
  f.panel.render(f.cx);
  await Promise.all(f.tasks);
  assert.equal(f.requests.filter(r => r?.args?.includes("status")).length, 3);
});

test("reactive machine changes refresh details once, unchanged renders do not poll", async () => {
  const f = fixture();
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  f.setMachines([{ name: "remote", layout: "ready" }]);
  f.panel.render(f.cx);
  await Promise.all(f.tasks);
  f.panel.render(f.cx);
  assert.equal(f.requests.filter(r => r === "machines").length, 2);
  assert.equal(f.panel.machines[0].name, "remote");
});

test("a new space becomes visible at the one-shot deadline even if Git is delayed", async () => {
  const f = fixture();
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  f.panel.refreshGit = () => {};
  f.setSpaces([space]);
  f.panel.render(f.cx);
  assert.equal(f.panel.ordered().length, 0);
  f.advance(1500);
  f.after[0].callback(f.cx);
  assert.equal(f.panel.ordered().length, 1);
});

test("closed Spaces ignores late machine responses and one-shot redraws", async () => {
  const f = fixture();
  f.panel.init({}, f.cx);
  f.setSpaces([space]);
  f.panel.checkNew(f.cx, false);
  f.panel.close();
  const before = f.notifications();
  await Promise.all(f.tasks);
  f.after[0].callback(f.cx);
  assert.equal(f.notifications(), before);
  assert.equal(f.panel.watching.size, 0);
});

test("local Git watcher refreshes on a filesystem event and stops publishing after close", async () => {
  let changed;
  const f = fixture({ watchFolder: () => new Promise(resolve => { changed = resolve; }) });
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  const key = JSON.stringify([null, "/project"]), reads = [];
  let delivered;
  const eventDelivered = new Promise(resolve => { delivered = resolve; });
  f.panel.git[key] = { repo: JSON.stringify([null, "/project/.git"]) };
  f.panel.refreshGit = keys => { reads.push(keys); if (reads.length === 2) delivered(); };
  f.panel.watch("/project/.git", f.cx);
  const watching = f.tasks.at(-1);
  assert.equal(reads.length, 1); // Initial reconciliation after registering watch.
  changed();
  await eventDelivered;
  assert.deepEqual(Array.from(reads[0]), [key]);
  f.panel.close();
  changed();
  await watching;
  assert.equal(reads.length, 2);
});

test("remote Git retains slow reconciliation for changes outside Herdr", async () => {
  const f = fixture();
  f.setSpaces([space]);
  f.panel.init({}, f.cx);
  await Promise.all(f.tasks);
  f.advance(60000);
  f.every[0].callback(f.cx);
  await Promise.all(f.tasks);
  assert.equal(f.requests.filter(r => r?.args?.includes("status")).length, 2);
});
