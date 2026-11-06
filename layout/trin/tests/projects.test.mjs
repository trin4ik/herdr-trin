import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const source = file => fs.readFileSync(new URL(`../mods/trin-projects/${file}`, import.meta.url), "utf8");
const data = await import(`data:text/javascript,${encodeURIComponent(source("data.js"))}`);
const { KEY, stateOf, adopt, updateProject, moveProject, removeProject, liveSpace, machineReady, launchArgs } = data;
const p = (id = "a", machine = null) => ({ id, name: `Project ${id}`, folder: `/projects/${id}`, machine, space: `s-${id}`, color: "ansi6" });
const s = (id = "a", machine = null) => ({ id: `s-${id}`, name: `live ${id}`, folder: "/old", machine, active: id === "a", tabs: [{ id: `t-${id}`, active: true }] });
const state = (projects = [p()]) => ({ version: 1, projects, selected: projects[0]?.id ?? null, ignored: [] });
const plain = value => JSON.parse(JSON.stringify(value));

function element() {
  const st = { childNodes: [], attrs: {}, handlers: {} };
  const el = new Proxy(st, { get(t, k) {
    if (k in t) return t[k];
    return (...a) => {
      if (k === "when") { if (a[0]) a[1](el); }
      else if (k === "child" || k === "content") st.childNodes.push(a[0]);
      else if (k === "children") st.childNodes.push(...a[0]);
      else if (k.startsWith("on_")) st.handlers[k + (typeof a[0] === "string" ? ":" + a[0] : "")] = a.at(-1);
      else st.attrs[k] = a;
      return el;
    };
  } }); return el;
}
const nodes = el => el && typeof el === "object" ? [el, ...el.childNodes.flatMap(nodes)] : [];
const byID = (el, id) => nodes(el).find(n => n.attrs.id?.[0] === id);
const text = el => el && typeof el === "object" ? el.childNodes.map(text).join("") : String(el ?? "");

function fixture(initial = state(), initialSpaces = [s()]) {
  let saved = plain(initial), sp = plain(initialSpaces), focus = { space: sp[0]?.id, tab: sp[0]?.tabs[0]?.id, pane: "pane" };
  let ms = [{ name: null, layout: "ready" }, { name: "remote", layout: "ready" }];
  const calls = [], overlays = [], notices = [], tree = { id: "root", children: [{ id: "project-workspace", active: "main" }, { id: "panel", hidden: true }] };
  const context = { ...data, View: class {}, div: element, svg: element, h_flex: element, v_flex: element,
    seeds: {}, shared: key => key === KEY ? saved : {}, read: () => saved, write: next => { saved = next; },
    props: () => ({}), spaces: () => sp, machines: () => ms, agents: () => [], focused: () => focus,
    theme: () => ({ foreground: "#ffffff", muted: "#999999", dim: "#777777" }), layout: () => tree,
    localStorage: { getItem: () => null, setItem() {} },
    notify: n => notices.push(n), openOverlay: spec => overlays.push(spec), overlay: (surface, args) => overlays.push({ surface, args }),
    command() {}, renderSurface() {}, initSurface() {}, disposeRemoteFolderPicker() {}, nextCommand: () => new Promise(() => {}),
    hc: async () => ({ code: 0, stdout: '{"machines":[]}' }), knownAgents: async () => [],
    exec: async (_cmd, args) => { calls.push({ exec: args }); return { code: 0, stdout: '{"space":"created","tab":"new-tab","terminal":"new-terminal"}' }; },
    run: (name, args) => {
      calls.push({ name, args });
      if (name === "space.select") { sp.forEach(s => { s.active = s.id === args.id; }); focus = { ...focus, space: args.id, tab: sp.find(s => s.active)?.tabs[0]?.id }; }
      if (name === "layout.select") tree.children.find(n => n.id === args.stack).active = args.child;
      if (name === "layout.hide") tree.children.find(n => n.id === args.id).hidden = true;
    }
  };
  vm.runInNewContext(source("main.js").replace(/^import .*;\n/gm, "").replace("export default class", "class") + "\nthis.Panel = Projects;", context);
  const panel = new context.Panel();
  Object.assign(panel, { closed: false, machineNames: [], machineSignature: JSON.stringify(ms), folded: new Set(), nativeFocus: JSON.stringify([focus.space, focus.tab, focus.pane]) });
  return { panel, context, calls, overlays, notices, tree, get saved() { return saved; }, setSpaces: next => { sp = next; }, setMachines: next => { ms = next; }, setFocus: next => { focus = next; } };
}
const cx = { notify() {}, stop_propagation() {} };

test("project registry survives closing every session and changing all terminal cwd/name facts", () => {
  const original = state();
  assert.equal(adopt(original, []), original);
  assert.equal(adopt(original, [{ ...s(), name: "Other repo", folder: "/elsewhere", branch: "feature" }]), original);
  assert.equal(original.projects[0].folder, "/projects/a");
});
test("existing Space IDs are adopted exactly once, preserving confirmed roots and colors", () => {
  const first = adopt(state([]), [s()], { "s-a": { name: "Stable", folder: "/confirmed" } }, { "s-a": "ansi3" });
  assert.equal(first.projects[0].name, "Stable"); assert.equal(first.projects[0].folder, "/confirmed"); assert.equal(first.projects[0].color, "ansi3");
  assert.equal(adopt(first, [s()]), first);
});
test("offline/loading machines never delete project identity or native binding", () => {
  const original = state([p("a", "remote")]);
  assert.equal(adopt(original, []), original);
  for (const layout of ["loading", "unavailable"]) assert.equal(machineReady(original.projects[0], [{ name: "remote", layout }]), false);
  assert.equal(liveSpace(original.projects[0], []), undefined);
});
test("corrupt/unknown registry fails closed instead of resetting user projects", () => {
  assert.throws(() => stateOf({ version: 2 }), /формат/);
  assert.throws(() => stateOf({ ...state(), projects: [p(), p()] }), /Повреждён/);
  assert.equal(stateOf(null).version, 1);
});
test("reordering is persistent, machine-local and stable under missing targets", () => {
  const original = state([p("a"), p("b"), p("c", "remote")]);
  assert.deepEqual(moveProject(original, "a", "b").projects.map(p => p.id), ["b", "a", "c"]);
  assert.equal(moveProject(original, "a", "c"), original);
  assert.equal(moveProject(original, "missing", "b"), original);
});
test("deleting a record neither deletes a directory nor reimports the old Space", () => {
  const next = removeProject(state(), "a");
  assert.equal(next.projects.length, 0); assert.deepEqual(next.ignored, ["s-a"]);
  assert.equal(adopt(next, [s()]), next);
});
test("launch arguments enforce an explicit root/machine, even for home/global projects", () => {
  assert.deepEqual(launchArgs({ ...p(), folder: "/Users/me" }), { name: "Project a", folder: "/Users/me", machine: "" });
  assert.equal(launchArgs(p("a", "remote"), { argv: ["agent"] }).machine, "remote");
  assert.throws(() => launchArgs({ ...p(), folder: "~" }), /абсолютную/);
});
test("new tabs in existing spaces always pass project root, not live folder", async () => {
  const f = fixture(); await f.panel.newTab();
  const call = f.calls.find(c => c.name === "tab.new");
  assert.deepEqual(plain(call.args), { space: "s-a", folder: "/projects/a" });
  assert.equal(f.calls.filter(c => c.exec).length, 0);
});
test("an empty project launches once through acknowledged CLI IDs without a dummy tab", async () => {
  const f = fixture(state(), []); await f.panel.newTab();
  const args = f.calls.find(c => c.exec).exec;
  assert.equal(args[1], "space.new");
  assert.deepEqual(JSON.parse(args[2]), { name: "Project a", folder: "/projects/a", machine: "" });
  assert.equal(f.saved.projects[0].space, "created");
  assert.equal(f.saved.projects.length, 1);
});
test("first agent session keeps the project name and passes a separate initial tab name", async () => {
  const f = fixture(state(), []); await f.panel.newTab("a", { name: "Agent", argv: ["agent"] });
  const args = JSON.parse(f.calls.find(c => c.exec).exec[2]);
  assert.equal(args.name, "Project a"); assert.equal(args.tab, "Agent"); assert.deepEqual(args.argv, ["agent"]);
});
test("launch failure preserves project, binding and explicit error; no silent retry", async () => {
  const f = fixture(state(), []);
  f.context.exec = async () => ({ code: 1, stdout: '{"error":"folder missing"}' });
  await assert.rejects(f.panel.newTab(), /folder missing/);
  assert.equal(f.saved.projects[0].space, "s-a"); assert.equal(f.panel.launching, null);
});
test("pending launch cannot be double-imported; user selection and edited root win late reply", async () => {
  const f = fixture(state([p(), p("b")]), []); let finish;
  f.context.exec = () => new Promise(resolve => { finish = resolve; });
  const pending = f.panel.newTab("a");
  f.setSpaces([{ ...s(), id: "created" }]); f.panel.sync();
  assert.equal(f.saved.projects.length, 2);
  f.context.write(updateProject(f.saved, "a", { folder: "/edited" })); f.panel.select("b");
  finish({ code: 0, stdout: '{"space":"created"}' }); await pending;
  assert.equal(f.saved.selected, "b"); assert.equal(f.saved.projects[0].folder, "/edited");
  assert.equal(f.saved.projects[0].space, "created");
});
test("close cleanup rejects late registry writes", async () => {
  const f = fixture(state(), []); let finish;
  f.context.exec = () => new Promise(resolve => { finish = resolve; });
  const pending = f.panel.newTab(); f.panel.close();
  finish({ code: 0, stdout: '{"space":"created"}' }); await pending;
  assert.equal(f.saved.projects[0].space, "s-a");
  assert.ok(f.saved.projects[0].pending, "saved request permits recovery by a new owner");
});

test("reload recovers a unique in-flight native launch without changing permanent project identity", () => {
  const original = state([{ ...p(), pending: { at: 1000, before: ["older"], name: "Original request", folder: "/requested" } }]);
  const native = { ...s(), id: "created", name: "Original request", folder: "/requested" };
  const recovered = adopt(original, [native], {}, {}, 2000);
  assert.equal(recovered.projects.length, 1); assert.equal(recovered.projects[0].id, "a");
  assert.equal(recovered.projects[0].folder, "/projects/a"); assert.equal(recovered.projects[0].space, "created"); assert.equal(recovered.projects[0].pending, null);
});
test("reload never guesses an ambiguous launch; bounded lock does not expire durable recovery", () => {
  const original = state([{ ...p(), pending: { at: 1000, before: [], name: "Request", folder: "/requested" } }]);
  const natives = ["x", "y"].map(id => ({ ...s(), id, name: "Request", folder: "/requested" }));
  assert.equal(adopt(original, natives, {}, {}, 2000), original);
  const expired = adopt(original, [], {}, {}, 37000);
  assert.equal(expired, original); assert.equal(data.launchLocked(original.projects[0], 37000), false);
  const recovered = adopt(expired, [natives[0]], {}, {}, 500000);
  assert.equal(recovered.projects[0].space, "x"); assert.equal(recovered.projects.length, 1);
});
test("a failed CLI acknowledgement keeps durable correlation across an offline interval", async () => {
  const f = fixture(state(), []); f.context.exec = async () => { throw new Error("CLI timed out"); };
  await assert.rejects(f.panel.newTab(), /timed out/);
  assert.equal(f.saved.projects[0].pending.failed, true);
  f.setMachines([{ name: null, layout: "unavailable" }]); f.panel.sync();
  assert.ok(f.saved.projects[0].pending);
  f.setMachines([{ name: null, layout: "ready" }]); f.setSpaces([{ ...s(), id: "late", name: "Project a", folder: "/projects/a" }]); f.panel.sync();
  assert.equal(f.saved.projects[0].space, "late"); assert.equal(f.saved.projects.length, 1);
});
test("reload recovery restores selected B natively when late launch A owns the active terminal", () => {
  const a = { ...p(), pending: { at: 1000, before: [], name: "Project a", folder: "/projects/a" } };
  const initial = { ...state([a, p("b")]), selected: "b" };
  const f = fixture(initial, [{ ...s(), id: "late-a", name: "Project a", folder: "/projects/a" }, s("b")]);
  f.panel.sync();
  assert.equal(f.saved.selected, "b"); assert.equal(f.saved.projects[0].space, "late-a");
  assert.equal(f.calls.find(c => c.name === "space.select").args.id, "s-b");
  assert.equal(f.context.spaces().find(s => s.active).id, "s-b");
});
test("unavailable machine blocks launches and destructive project actions", async () => {
  const f = fixture(); f.setMachines([{ name: null, layout: "unavailable" }]);
  await assert.rejects(f.panel.newTab(), /подключения/);
  await assert.rejects(f.panel.act("remove--a", cx), /подключения/);
  assert.equal(f.calls.length, 0);
});
test("selecting an empty project shows its empty workspace, not the previous terminal", () => {
  const f = fixture(state([p(), p("b")]), [s()]); f.panel.select("b");
  assert.equal(f.saved.selected, "b"); assert.equal(f.tree.children[0].active, "project-empty");
  assert.equal(f.calls.filter(c => c.name === "space.new" || c.exec).length, 0);
  f.panel.select("a"); assert.equal(f.tree.children[0].active, "main");
});
test("close, split and tab-navigation shortcuts on empty projects cannot affect old focus", async () => {
  const f = fixture(state([p(), p("b")]), [s()]); f.panel.select("b"); f.calls.length = 0;
  for (const action of ["close-tab", "close-pane", "split-right", "split-down", "zoom", "tab-next", "tab-prev", "tab-index--1", "rename-tab", "pane-go--right"]) await f.panel.act(action, cx);
  assert.equal(f.calls.length, 0);
});
test("closing the last session keeps selected project and switches to empty state", () => {
  const f = fixture(); f.setSpaces([]); f.panel.sync();
  assert.equal(f.saved.projects.length, 1); assert.equal(f.saved.selected, "a"); assert.equal(f.tree.children[0].active, "project-empty");
});
test("native focus changes reconcile project selection without rewriting roots", () => {
  const f = fixture(state([p(), p("b")]), [s(), { ...s("b"), active: true }]);
  f.setSpaces([{ ...s(), active: false }, { ...s("b"), active: true }]);
  f.setFocus({ space: "s-b", tab: "t-b", pane: "pane-b" }); f.panel.sync();
  assert.equal(f.saved.selected, "b"); assert.equal(f.saved.projects[1].folder, "/projects/b");
});
test("project rows never show branch/cwd, retain empty entries and machine-local ordering", () => {
  const f = fixture(state([p(), p("b", "remote"), p("c")]), [{ ...s(), branch: "feature/unrelated" }]);
  assert.deepEqual(plain(f.panel.ordered(f.saved).map(p => p.id)), ["a", "c", "b"]);
  const row = f.panel.row(f.saved.projects[0], {}, false);
  assert.equal(text(row), "PProject a1"); assert.doesNotMatch(text(row), /feature|old/);
  assert.equal(text(f.panel.row(f.saved.projects[1], {}, false)), "PProject b0");
});
test("project rail uses permanent avatar/color and selecting an empty project does not launch", () => {
  const f = fixture(state([p(), p("b")]), [s()]);
  const row = f.panel.row(f.saved.projects[1], {}, true);
  assert.equal(text(row), "P"); assert.match(row.attrs.tooltip[0], /\/projects\/b/);
  row.handlers.on_click(); assert.equal(f.saved.selected, "b"); assert.equal(f.calls.filter(c => c.exec).length, 0);
});
test("project picker/menu commands encode IDs without the reserved command argument colon", async () => {
  const f = fixture(); await f.panel.act("select--" + encodeURIComponent("a"), cx);
  assert.equal(f.saved.selected, "a");
  assert.match(source("store.js"), /"--" \+ encodeURIComponent/);
});
test("project UI uses retained inputs, offline gating, local picker and explicit delete confirmation", () => {
  const ui = source("ui.js") + source("form.js");
  assert.match(ui, /InputState.new/); assert.match(ui, /pickFolder/); assert.match(ui, /confirm-remove/);
  assert.match(ui, /available && !s/); assert.match(ui, /surface !== "empty"/);
  assert.doesNotMatch(source("main.js"), /timer\.every|watchFolder|gitTail|git.*status/);
});

test("late agent discovery after close or changing project cannot start a session", async () => {
  for (const abandon of [f => f.panel.close(), f => f.panel.select("b")]) {
    const f = fixture(state([p(), p("b")]), []); let finish;
    f.context.knownAgents = () => new Promise(resolve => { finish = resolve; });
    const jobs = [], context = { ...cx, spawn: fn => { jobs.push(fn(context)); } };
    f.panel.startLaunch("a", "agent", context); abandon(f);
    finish([{ key: "agent", installed: true, run: ["agent"] }]); await Promise.all(jobs);
    assert.equal(f.calls.filter(c => c.exec || c.name === "tab.new").length, 0);
  }
});
test("bounded launch job does not block project navigation or enqueue another launch", async () => {
  const f = fixture(state([p(), p("b")]), []); let finish;
  f.context.exec = () => new Promise(resolve => { finish = resolve; });
  const jobs = [], context = { ...cx, spawn: fn => { jobs.push(fn(context)); } };
  await f.panel.act("new-tab--a", context);
  assert.equal(f.panel.jobBusy, true);
  await f.panel.act("select--b", context); assert.equal(f.saved.selected, "b");
  assert.throws(() => f.panel.startLaunch("b", null, context), /запускается/);
  finish({ code: 0, stdout: '{"space":"created"}' }); await Promise.all(jobs);
  assert.equal(f.saved.selected, "b"); assert.equal(f.panel.jobBusy, false);
});
test("only the always-visible status surface consumes commands, including with hidden sidebar", () => {
  const f = fixture(); let started = 0;
  const context = { ...cx, spawn: () => { started++; } };
  f.context.props = () => ({}); f.panel.init({}, context); assert.equal(started, 0);
  f.context.props = () => ({ surface: "status" }); f.panel.init({}, context); assert.equal(started, 1);
});
test("all project hotkeys are scoped to trin and cover empty-context safety", () => {
  const keys = JSON.parse(fs.readFileSync(new URL("../keys.json", import.meta.url), "utf8"));
  assert.deepEqual(Object.keys(keys), ["layout:trin"]);
  for (const key of ["cmd-t", "cmd-w", "cmd-shift-w", "cmd-d", "cmd-shift-d", "ctrl-tab", "cmd-1", "ctrl-1", "cmd-p"]) assert.match(keys["layout:trin"][key], /^trin-projects\./);
  const layout = JSON.parse(fs.readFileSync(new URL("../layouts/trin.json", import.meta.url), "utf8"));
  const walk = n => [n, ...(n.children ?? []).flatMap(walk)];
  assert.equal(walk(layout).find(n => n.id === "status").content.props.surface, "status");
});
test("direct remote browse opens only a form for a known machine, never a terminal", async () => {
  const f = fixture(); await f.panel.act("browse--remote", cx);
  assert.equal(f.overlays[0].surface, "form"); assert.deepEqual(plain(f.overlays[0].args), { machine: "remote", browse: true });
  assert.equal(f.calls.filter(c => c.exec || c.name === "space.new" || c.name === "tab.new").length, 0);
  await assert.rejects(f.panel.act("browse--unknown", cx), /добавленную/);
});

function surfaceFixture(surface, projectID = null) {
  let saved = state([p()]);
  const properties = { surface, project: projectID, overlay: "test-overlay" }, commands = [], overlays = [], inputs = [], jobs = [];
  const context = { ...data, div: element, svg: element, v_flex: element, h_flex: element,
    Button: { new: id => element().id(id) }, Select: { new: id => element().id(id) }, Popup: { new: (id, trigger) => element().id(id).child(trigger) },
    Input: { new: element }, InputState: { new: opts => {
      let value = opts.value ?? "";
      const input = { value: () => value, set_value: next => { value = next; }, on() {}, focus() {} };
      inputs.push(input); return input;
    } },
    props: () => properties, read: () => saved, write: next => { saved = next; },
    current: () => saved.projects.find(p => p.id === saved.selected), target: () => saved.projects.find(p => p.id === properties.project),
    live: () => undefined, ready: () => true, agents: () => [], window: () => ({ platform: "macos" }), keys: () => [],
    theme: () => ({ foreground: "#ffffff", muted: "#999999", dim: "#777777" }),
    machines: () => [{ name: null, layout: "ready" }, { name: "remote", layout: "unavailable" }], home: () => "/Users/me",
    closeOverlay: id => overlays.push(id), overlay() {}, run: name => commands.push(name), command: (action, id) => commands.push({ action, id }),
    knownAgents: async () => [], pickFolder: async () => null,
    openRemoteFolderPicker: () => {}, renderRemoteFolderPicker: () => element(), cancelRemoteFolderPicker: () => {}
  };
  const flatten = file => source(file).replace(/^import .*;\n/gm, "").replaceAll("export function", "function");
  vm.runInNewContext(flatten("form.js") + "\n" + flatten("ui.js") + "\nthis.initUI = initSurface; this.renderUI = renderSurface;", context);
  const view = {}, contextCX = { ...cx, focus_handle: () => ({ focus() {} }), spawn: fn => { const job = fn(contextCX); jobs.push(job); return job; } };
  context.initUI(view, contextCX);
  return { view, context, commands, overlays, inputs, jobs, cx: contextCX, get saved() { return saved; }, render: () => context.renderUI(view, { notify() { throw new Error("expired render context"); } }) };
}
test("new-project form saves permanent identity without creating any terminal", () => {
  const f = surfaceFixture("form"); f.view.name.set_value("Global"); f.view.folder.set_value("/Users/global");
  byID(f.render(), "project-form-save").handlers.on_click({}, f.cx);
  const created = f.saved.projects[1]; assert.equal(created.name, "Global"); assert.equal(created.folder, "/Users/global"); assert.equal(created.space, null);
  assert.equal(f.commands.length, 1); assert.equal(f.commands[0].action, "select");
});
test("editing a project changes its fixed name/root but preserves native session binding", () => {
  const f = surfaceFixture("form", "a"); f.view.name.set_value("Fixed"); f.view.folder.set_value("/new-root");
  byID(f.render(), "project-form-save").handlers.on_click({}, f.cx);
  assert.equal(f.saved.projects[0].space, "s-a"); assert.equal(f.saved.projects[0].color, "ansi6");
  assert.equal(f.saved.projects[0].name, "Fixed"); assert.equal(f.saved.projects[0].folder, "/new-root");
});
test("project form rejects relative and duplicate roots without mutating the registry", () => {
  const f = surfaceFixture("form"); f.view.name.set_value("Duplicate");
  for (const path of ["", "relative/path", "/projects/a"]) {
    f.view.folder.set_value(path); byID(f.render(), "project-form-save").handlers.on_click({}, f.cx);
    assert.equal(f.saved.projects.length, 1); assert.ok(f.view.fieldErrors.folder);
  }
});
test("empty-project workspace shows its own root and never steals keyboard focus on init", () => {
  const f = surfaceFixture("empty"); const root = f.render();
  assert.match(text(root), /Project a/); assert.match(text(root), /\/projects\/a/);
  byID(root, "project-start").handlers.on_click(); assert.equal(f.commands[0].action, "new-tab");
});
test("project picker includes empty projects and preserves their IDs on selection", () => {
  const f = surfaceFixture("picker"), root = f.render();
  byID(root, "pick-a").handlers.on_click(); assert.deepEqual(f.commands[0], { action: "select", id: "a" });
});

function openMachineList(f) {
  byID(f.render(), "project-machine-select").handlers.on_open_change(true, f.cx);
  return f.render();
}
function selectMachine(f, index) {
  byID(openMachineList(f), `project-machine-option-${index}`).handlers.on_click({}, f.cx);
}
test("native machine dropdown uses live event contexts and preserves a path draft per machine", () => {
  const f = surfaceFixture("form");
  assert.equal(f.view.folder.value(), "", "new project must not silently default to the Mac home");
  f.view.name.set_value("Stable name"); f.view.folder.set_value("/Users/me/local project");
  selectMachine(f, 1); assert.equal(f.view.machine, "remote"); assert.equal(f.view.folder.value(), "");
  assert.ok(byID(f.render(), "project-pick-folder"), "remote machines also offer the folder chooser");
  f.view.folder.set_value("/home/remote/project");
  selectMachine(f, 0); assert.equal(f.view.folder.value(), "/Users/me/local project");
  selectMachine(f, 1); assert.equal(f.view.folder.value(), "/home/remote/project");
  assert.equal(f.view.name.value(), "Stable name"); assert.equal(f.view.machineOpen, false);
});
test("machine dropdown has arrow/Enter/Escape behavior without closing the form", () => {
  const f = surfaceFixture("form");
  let list = byID(openMachineList(f), "project-machine-options");
  list.handlers.on_key_down({ key: "down" }, f.cx);
  assert.equal(f.view.machineAt, 1);
  list = byID(f.render(), "project-machine-options");
  list.handlers.on_key_down({ key: "enter" }, f.cx);
  assert.equal(f.view.machine, "remote"); assert.equal(f.view.machineOpen, false);
  list = byID(openMachineList(f), "project-machine-options");
  list.handlers.on_key_down({ key: "escape" }, f.cx);
  assert.equal(f.view.machineOpen, false); assert.equal(f.overlays.length, 0);
});
test("folder picker cannot overwrite a different machine or a newer typed value", async () => {
  for (const switchAway of [true, false]) {
    const f = surfaceFixture("form"); let finish;
    f.context.pickFolder = () => new Promise(resolve => { finish = resolve; });
    byID(f.render(), "project-pick-folder").handlers.on_click({}, f.cx);
    assert.equal(f.view.picking, true);
    if (switchAway) selectMachine(f, 1);
    f.view.folder.set_value("/newer/typed");
    finish("/Users/me/picked"); await Promise.all(f.jobs);
    assert.equal(f.view.folder.value(), "/newer/typed");
    assert.equal(f.view.picking, false);
  }
});
test("folder picker cancellation, error and close keep retained user input safe", async () => {
  for (const result of ["cancel", "error", "closed"]) {
    const f = surfaceFixture("form"); f.view.folder.set_value("/original"); let finish;
    f.context.pickFolder = result === "error" ? async () => { throw new Error("picker unavailable"); } : () => new Promise(resolve => { finish = resolve; });
    byID(f.render(), "project-pick-folder").handlers.on_click({}, f.cx);
    if (result === "closed") f.view.closed = true;
    if (finish) finish(result === "cancel" ? null : "/late");
    await Promise.all(f.jobs);
    assert.equal(f.view.folder.value(), "/original");
    if (result === "error") assert.match(f.view.error, /Введите путь вручную/);
  }
});
test("local home shorthand expands only on save; remote shorthand is rejected", () => {
  const local = surfaceFixture("form"); local.view.name.set_value("Local"); local.view.folder.set_value("~/my project");
  byID(local.render(), "project-form-save").handlers.on_click({}, local.cx);
  assert.equal(local.saved.projects[1].folder, "/Users/me/my project");
  const remote = surfaceFixture("form"); selectMachine(remote, 1);
  remote.view.name.set_value("Remote"); remote.view.folder.set_value("~/project");
  byID(remote.render(), "project-form-save").handlers.on_click({}, remote.cx);
  assert.equal(remote.saved.projects.length, 1); assert.match(remote.view.fieldErrors.folder, /полный путь/);
});
test("remote form saves the selected machine and its own path, including when offline", () => {
  const f = surfaceFixture("form"); selectMachine(f, 1); f.view.name.set_value("Remote project"); f.view.folder.set_value("/home/me/проект с пробелами");
  byID(f.render(), "project-form-save").handlers.on_click({}, f.cx);
  assert.equal(f.saved.projects[1].machine, "remote"); assert.equal(f.saved.projects[1].folder, "/home/me/проект с пробелами");
});
test("saving a selected folder preserves trailing filename spaces exactly", () => {
  const f = surfaceFixture("form"); selectMachine(f, 1); f.view.name.set_value("Exact path"); f.view.folder.set_value("/home/me/folder ");
  byID(f.render(), "project-form-save").handlers.on_click({}, f.cx);
  assert.equal(f.saved.projects[1].folder, "/home/me/folder ");
});
test("directory basenames support POSIX, Windows, roots and significant filename spaces", () => {
  for (const [path, name] of [["/home/me/project/", "project"], ["/", "/"], ["/home/project ", "project "], ["C:\\work\\repo\\", "repo"], ["/home/a\\b", "a\\b"]])
    assert.equal(data.directoryName(path), name);
});
test("empty project name is derived on save without altering an explicit name", () => {
  for (const name of ["", "Custom"]) {
    const f = surfaceFixture("form"); f.view.name.set_value(name); f.view.folder.set_value("/home/me/chosen");
    byID(f.render(), "project-form-save").handlers.on_click({}, f.cx);
    assert.equal(f.saved.projects[1].name, name || "chosen");
  }
});
test("local directory selection autofills only a blank project name", async () => {
  for (const name of ["", "Custom"]) {
    const f = surfaceFixture("form"); f.view.name.set_value(name); f.context.pickFolder = async () => "/Users/me/chosen";
    byID(f.render(), "project-pick-folder").handlers.on_click({}, f.cx); await Promise.all(f.jobs);
    assert.equal(f.view.name.value(), name || "chosen");
  }
});
test("form naturally sizes its content and keeps validation inside the viewport cap", () => {
  const f = surfaceFixture("form"), root = f.render();
  assert.equal(root.attrs.h_full, undefined); assert.equal(root.attrs.max_h[0], 600);
  assert.equal(root.attrs.h[0], 382);
  assert.equal(root.attrs.flex_1, undefined);
  const store = source("store.js");
  assert.match(store, /surface === "form" \? \{\}/);
  assert.doesNotMatch(store, /500/);
  assert.match(text(root), /Создать проект/);
  assert.equal(byID(root, "project-machine-trigger").attrs.h[0], 36);
  f.view.fieldErrors = { name: "Введите название", folder: "Выберите папку" };
  assert.equal(f.render().attrs.h[0], 458);
  f.context.window.viewport_size = () => ({ height: 260, width: 800 });
  assert.equal(f.render().attrs.h[0], 212);
  assert.ok(f.render().attrs.overflow_y_scrollbar);
});
