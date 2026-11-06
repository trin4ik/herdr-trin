import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const source = name => fs.readFileSync(new URL(`../mods/trin-projects/${name}`, import.meta.url), "utf8");
const browser = await import(`data:text/javascript,${encodeURIComponent(source("folder-browser.js"))}`);
const { FolderBrowser, folderParent, folderChild } = browser;
const data = await import(`data:text/javascript,${encodeURIComponent(source("data.js"))}`);
const tick = () => new Promise(resolve => setImmediate(resolve));
const snapshot = (path = "/home/me", entries = []) => ({ ok: true, path, realPath: path, home: "/home/me", entries, nextCursor: null, limited: false });
const entry = name => ({ name, link: false, target: null, error: null });
const clean = value => JSON.parse(JSON.stringify(value));

function modelFixture() {
  const pending = [], jobs = [];
  const read = request => new Promise((resolve, reject) => pending.push({ request, resolve, reject }));
  const state = { alive: true };
  const cx = { notify() {}, spawn: fn => { const job = fn(cx); jobs.push(job); } };
  return { model: new FolderBrowser(read, () => state.alive), pending, jobs, cx, state };
}

test("folder navigation preserves logical link paths, including root parent", () => {
  assert.equal(folderParent("/home/me/projects"), "/home/me");
  assert.equal(folderParent("/"), "/");
  assert.equal(folderChild("/", "mnt"), "/mnt");
  assert.equal(folderChild("/home/me/projects", "с пробелами"), "/home/me/projects/с пробелами");
});
test("requests are serialized, coalesced to latest intent and discard obsolete results", async () => {
  const f = modelFixture();
  f.model.request("a", "/one", f.cx);
  f.model.request("a", "/two", f.cx);
  f.model.request("b", "/three", f.cx);
  assert.equal(f.pending.length, 1);
  f.pending[0].resolve(snapshot("/one")); await tick();
  assert.equal(f.model.snapshot, null); assert.equal(f.pending.length, 2);
  assert.equal(f.pending[1].request.machine, "b"); assert.equal(f.pending[1].request.path, "/three");
  f.pending[1].resolve(snapshot("/three")); await Promise.all(f.jobs);
  assert.equal(f.model.snapshot.path, "/three"); assert.equal(f.model.loading, false);
});
test("cancel/reopen reuses the same outstanding read rather than starting parallel jobs", async () => {
  const f = modelFixture(); f.model.request("a", "/old", f.cx); f.model.cancel(); f.model.reset(); f.model.request("a", "/new", f.cx);
  assert.equal(f.pending.length, 1);
  f.pending[0].resolve(snapshot("/old")); await tick();
  assert.equal(f.pending.length, 2); assert.equal(f.model.snapshot, null);
  f.pending[1].resolve(snapshot("/new")); await Promise.all(f.jobs);
  assert.equal(f.model.snapshot.path, "/new");
});
test("closed view discards late replies and never starts queued reads", async () => {
  const f = modelFixture(); f.model.request("a", "/old", f.cx); f.model.request("a", "/new", f.cx); f.state.alive = false;
  f.pending[0].resolve(snapshot("/old")); await Promise.all(f.jobs);
  assert.equal(f.model.snapshot, null); assert.equal(f.pending.length, 1);
});
test("read errors are explicit, preserve the last snapshot and retry only on request", async () => {
  const f = modelFixture(); f.model.request("a", "/good", f.cx); f.pending[0].resolve(snapshot("/good")); await Promise.all(f.jobs);
  f.model.request("a", "/bad", f.cx); f.pending[1].reject(new Error("permission")); await Promise.all(f.jobs);
  assert.equal(f.model.snapshot.path, "/good"); assert.equal(f.model.error, "permission"); assert.equal(f.pending.length, 2);
  f.model.request("a", "/good", f.cx); f.pending[2].resolve(snapshot("/good")); await Promise.all(f.jobs);
  assert.equal(f.model.error, "");
});
test("pagination appends stable names without duplicates and caps retained rows", async () => {
  const f = modelFixture(); f.model.request("a", "/p", f.cx);
  f.pending[0].resolve({ ...snapshot("/p", [entry("a"), entry("b")]), nextCursor: "b" }); await Promise.all(f.jobs);
  f.model.request("a", "/p", f.cx, {}, "more");
  assert.equal(f.pending[1].request.cursor, "b");
  f.pending[1].resolve(snapshot("/p", [entry("b"), entry("c")])); await Promise.all(f.jobs);
  assert.deepEqual(f.model.snapshot.entries.map(e => e.name), ["a", "b", "c"]);
  f.model.request("a", "/p", f.cx);
  f.pending[2].resolve({ ...snapshot("/p", Array.from({ length: 4200 }, (_, i) => entry(String(i)))), nextCursor: "more" }); await Promise.all(f.jobs);
  assert.equal(f.model.snapshot.entries.length, 4000); assert.equal(f.model.snapshot.limited, true); assert.equal(f.model.snapshot.nextCursor, null);
});

function element() {
  const st = { childNodes: [], attrs: {}, handlers: {} };
  const el = new Proxy(st, { get(t, key) {
    if (key in t) return t[key];
    return (...args) => {
      if (key === "when") { if (args[0]) args[1](el); }
      else if (key === "child" || key === "content") st.childNodes.push(args[0]);
      else if (key === "children") st.childNodes.push(...args[0]);
      else if (key.startsWith("on_")) st.handlers[key] = args.at(-1);
      else st.attrs[key] = args;
      return el;
    };
  } }); return el;
}
const nodes = e => e && typeof e === "object" ? [e, ...e.childNodes.flatMap(nodes)] : [];
const byID = (root, id) => nodes(root).find(n => n.attrs.id?.[0] === id);

function pickerFixture(initial = "") {
  const pending = [], jobs = [], handles = [], timers = [];
  const cx = { notify() {}, stop_propagation() {}, focus_handle: () => {
    const h = { released: false, focused: 0, focus() { assert.equal(h.released, false); this.focused++; }, release() { h.released = true; } }; handles.push(h); return h;
  }, timer: { after(ms, fn) { const task = { ms, cancelled: false, cancel() { this.cancelled = true; }, fire() { if (!this.cancelled) fn(cx); } }; timers.push(task); return task; } },
  spawn: fn => { const job = fn(cx); jobs.push(job); } };
  const input = value => {
    const events = {};
    return { released: false, value: () => value, set_value(next) { value = next; events.change?.({}, cx); }, focus() {}, release() { this.released = true; }, on: (name, fn) => { events[name] = fn; } };
  };
  const context = { ...browser, ...data, div: element, svg: element, v_flex: element, h_flex: element,
    Button: { new: id => element().id(id) }, InputState: { new: o => input(o.value) }, Input: { new: () => element() },
    Scrollbar: { vertical: () => element() }, VirtualListScrollHandle: { new: () => {
      const h = { released: false, scroll_to_item() { assert.equal(h.released, false); }, release() { h.released = true; } }; handles.push(h); return h;
    } },
    v_virtual_list: (id, count, _size, _key, render) => element().id(id).children(render({ start: 0, end: count })),
    createDirectoryReader: () => request => new Promise((resolve, reject) => pending.push({ request, resolve, reject })),
    directoryMessage: code => code, window: { viewport_size: () => ({ height: 800 }) }
  };
  vm.runInNewContext(source("remote-picker.js").replace(/^import .*;\n/gm, "").replaceAll("export function", "function")
    + "\nthis.open = openRemoteFolderPicker; this.render = renderRemoteFolderPicker; this.go = navigateRemoteFolder; this.cancel = cancelRemoteFolderPicker; this.choose = chooseRemoteFolder; this.dispose = disposeRemoteFolderPicker;", context);
  const view = { machine: "remote", name: input(""), folder: input(initial), folderDrafts: {}, fieldErrors: {}, error: "", pickVersion: 0, closed: false };
  context.open(view, cx);
  return { context, view, cx, jobs, pending, handles, timers, render: () => context.render(view, { foreground: "#eee", muted: "#aaa" }) };
}

test("remote folder choice revalidates and writes the logical path into the original form only", async () => {
  const f = pickerFixture();
  assert.equal(f.pending[0].request.path, "~"); assert.equal(byID(f.render(), "remote-folder-choose").attrs.disabled[0], true);
  f.pending[0].resolve({ ...snapshot("/home/me/projects", [entry("a")]), realPath: "/mnt/data/projects" }); await Promise.all(f.jobs);
  const root = f.render(); assert.equal(byID(root, "remote-folder-choose").attrs.disabled[0], false);
  byID(root, "remote-folder-choose").handlers.on_click({}, f.cx);
  assert.equal(f.pending[1].request.validate, true);
  assert.equal(f.view.folder.value(), "", "no early write before validation");
  f.pending[1].resolve(snapshot("/home/me/projects")); await Promise.all(f.jobs);
  assert.equal(f.view.folder.value(), "/home/me/projects"); assert.equal(f.view.remotePicker, null);
  assert.equal(f.view.name.value(), "projects");
  assert.ok(f.handles.every(h => h.released));
});
test("Home/Up/hidden and directory entry actions use the selected machine and logical paths", async () => {
  const f = pickerFixture("/home/me/projects"); f.pending[0].resolve(snapshot("/home/me/projects", [entry("a")])); await Promise.all(f.jobs);
  byID(f.render(), "remote-folder-hidden").handlers.on_click({}, f.cx);
  assert.equal(f.pending[1].request.hidden, true); assert.equal(f.pending[1].request.machine, "remote");
  f.pending[1].resolve(snapshot("/home/me/projects", [entry("a")])); await Promise.all(f.jobs);
  byID(f.render(), "remote-folders").handlers.on_item_click("/home/me/projects/a", f.cx);
  assert.equal(f.pending[2].request.path, "/home/me/projects/a");
  f.pending[2].resolve(snapshot("/home/me/projects/a")); await Promise.all(f.jobs);
  byID(f.render(), "remote-folder-up").handlers.on_click({}, f.cx); assert.equal(f.pending[3].request.path, "/home/me/projects");
  f.pending[3].resolve(snapshot("/home/me/projects")); await Promise.all(f.jobs);
  byID(f.render(), "remote-folder-home").handlers.on_click({}, f.cx); assert.equal(f.pending[4].request.path, "~");
  f.pending[4].resolve(snapshot()); await Promise.all(f.jobs);
});
test("broken links and stale row IDs cannot navigate, a dirty path cannot be accepted", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/home/me", [{ ...entry("broken"), link: true, error: "not_found" }])); await Promise.all(f.jobs);
  byID(f.render(), "remote-folders").handlers.on_item_click("/home/me/broken", f.cx);
  byID(f.render(), "remote-folders").handlers.on_item_click("/old/root/entry", f.cx);
  assert.equal(f.pending.length, 1);
  f.view.remotePicker.path.set_value("/typed/new"); f.context.choose(f.view, f.cx);
  assert.equal(f.pending.length, 1); assert.equal(byID(f.render(), "remote-folder-choose").attrs.disabled[0], true);
});
test("typing during read/validation and cancel cannot overwrite current input or form draft", async () => {
  const f = pickerFixture("/original"); f.view.remotePicker.path.set_value("/newer");
  f.pending[0].resolve(snapshot("/original")); await Promise.all(f.jobs);
  assert.equal(f.view.remotePicker.path.value(), "/newer");
  f.context.go(f.view, "/chosen", f.cx); f.pending[1].resolve(snapshot("/chosen")); await Promise.all(f.jobs);
  f.context.choose(f.view, f.cx); f.view.remotePicker.path.set_value("/changed-mind");
  f.pending[2].resolve(snapshot("/chosen")); await Promise.all(f.jobs);
  assert.equal(f.view.folder.value(), "/original"); assert.ok(f.view.remotePicker);
  byID(f.render(), "remote-folder-cancel").handlers.on_click({}, f.cx);
  assert.equal(f.view.remotePicker, null); assert.equal(f.view.folder.value(), "/original");
});
test("close during listing cannot publish or select a folder", async () => {
  const f = pickerFixture("/original"); f.view.closed = true;
  f.pending[0].resolve(snapshot("/late")); await Promise.all(f.jobs);
  assert.equal(f.view.folder.value(), "/original"); assert.equal(f.view.remoteBrowser.snapshot, null);
});
test("cancel/close release every picker-owned handle and drop callbacks retaining it", async () => {
  for (const close of [false, true]) {
    const f = pickerFixture("/original"), picker = f.view.remotePicker;
    if (close) { f.view.closed = true; f.context.dispose(f.view); } else f.context.cancel(f.view, f.cx);
    assert.ok(f.handles.every(h => h.released)); assert.equal(picker.path.released, true);
    f.pending[0].resolve(snapshot("/late")); await Promise.all(f.jobs);
    assert.equal(f.view.folder.value(), "/original"); assert.equal(f.view.remotePicker, null);
    f.view.remoteBrowser.onChoose(snapshot("/wrong"), {}, f.cx);
    assert.equal(f.view.folder.value(), "/original");
  }
});

test("transport uses argv JSON and Herdr machine routing, never interpolated shell commands", async () => {
  const calls = [], context = { readTextFile: async () => "# readonly Python", appHome: () => "/config",
    exec: async (program, args, options) => { calls.push({ program, args, options }); return { code: 0, stdout: JSON.stringify(snapshot("/safe")) }; } };
  vm.runInNewContext(source("remote-directory.js").replace(/^import .*;\n/gm, "").replaceAll("export function", "function").replaceAll("export const", "const")
    + "\nthis.factory = createDirectoryReader; this.parse = parseDirectoryResult;", context);
  const path = "/home/'; $(touch nope) / проект";
  await context.factory()({ machine: "remote", path, hidden: true, validate: true });
  assert.equal(calls[0].program, "python3"); assert.equal(calls[0].options.on, "remote");
  assert.deepEqual(JSON.parse(calls[0].args[2]), { path, hidden: true, validate: true });
  assert.throws(() => context.parse({ code: 0, stdout: "oops" }), /некорректный/);
  assert.throws(() => context.parse({ code: 0, stdout: '{"ok":false,"code":"permission"}' }), /Нет доступа/);
  assert.throws(() => context.parse({ code: 0, stdout: JSON.stringify({ ...snapshot(), entries: [{ name: "../bad", link: false }] }) }), /некорректный/);
});

test("address completion waits 350ms without blanking/blocking the confirmed list or stealing focus", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/p", [entry("Alpha"), entry("Beta")])); await Promise.all(f.jobs);
  const p = f.view.remotePicker, focusCount = p.listFocus.focused;
  p.path.set_value("/p/a"); p.path.set_value("/p/Al");
  assert.equal(f.timers[0].cancelled, true); assert.equal(f.timers[1].ms, 350); assert.equal(f.pending.length, 1);
  assert.ok(byID(f.render(), "remote-folder-Alpha")); assert.ok(byID(f.render(), "remote-folder-Beta"));
  assert.equal(p.model.loading, false);
  assert.equal(byID(f.render(), "remote-folder-Alpha").attrs.disabled[0], false);
  assert.equal(byID(f.render(), "remote-folder-content").attrs.opacity[0], 1);
  assert.ok(byID(f.render(), "remote-folder-loading").attrs.invisible);
  f.timers[1].fire(); assert.equal(f.pending[1].request.path, "/p"); assert.equal(f.pending[1].request.query, "Al");
  f.pending[1].resolve(snapshot("/p", [entry("Alpha"), entry("Alpine")])); await Promise.all(f.jobs);
  assert.equal(p.path.value(), "/p/Al"); assert.equal(p.listFocus.focused, focusCount);
  assert.ok(byID(f.render(), "remote-folder-Alpine"));
  assert.equal(byID(f.render(), "remote-folder-search"), undefined);
});

test("navigation retains rows and dimensions under an absolute loading layer, then replaces the snapshot", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/old", [entry("a"), entry("b")])); await Promise.all(f.jobs);
  const ready = f.render(), p = f.view.remotePicker;
  f.context.go(f.view, "/next", f.cx);
  const loading = f.render(), content = byID(loading, "remote-folder-content"), layer = byID(loading, "remote-folder-loading");
  assert.ok(byID(loading, "remote-folder-a")); assert.ok(byID(loading, "remote-folder-b"));
  assert.equal(loading.attrs.h[0], ready.attrs.h[0]); assert.equal(content.attrs.opacity[0], 0.3);
  assert.deepEqual(clean(content.attrs.transition), ["opacity", 140]); assert.ok(layer.attrs.absolute); assert.ok(layer.attrs.inset_0);
  assert.equal(layer.attrs.opacity[0], 1); assert.equal(layer.attrs.invisible, undefined);
  byID(loading, "remote-folders").handlers.on_item_click("/old/a", f.cx);
  assert.equal(f.pending.length, 2, "dimmed stale rows cannot navigate while reading");
  f.pending[1].resolve(snapshot("/next", [entry("new")])); await Promise.all(f.jobs);
  const next = f.render(); assert.equal(byID(next, "remote-folder-a"), undefined); assert.ok(byID(next, "remote-folder-new"));
  assert.equal(byID(next, "remote-folder-content").attrs.opacity[0], 1);
  assert.ok(byID(next, "remote-folder-loading").attrs.invisible); assert.equal(p.path.value(), "/next");
});
test("hidden and pagination preserve all confirmed rows, including the more row, while loading", async () => {
  const f = pickerFixture(); f.pending[0].resolve({ ...snapshot("/p", [entry("a")]), nextCursor: "a" }); await Promise.all(f.jobs);
  byID(f.render(), "remote-folders").handlers.on_item_click("more", f.cx);
  assert.ok(byID(f.render(), "remote-folder-a")); assert.equal(byID(f.render(), "remote-folders").childNodes.length, 2);
  f.pending[1].resolve(snapshot("/p", [entry("b")])); await Promise.all(f.jobs);
  byID(f.render(), "remote-folder-hidden").handlers.on_click({}, f.cx);
  assert.ok(byID(f.render(), "remote-folder-a")); assert.ok(byID(f.render(), "remote-folder-b"));
  assert.equal(byID(f.render(), "remote-folder-loading").attrs.opacity[0], 1);
  f.pending[2].resolve(snapshot("/p", [entry("a"), entry("b"), entry(".hidden")])); await Promise.all(f.jobs);
  assert.ok(byID(f.render(), "remote-folder-.hidden"));
});
test("confirmed rows remain clickable during debounce and navigation cancels the pending filter", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/p", [entry("chosen")])); await Promise.all(f.jobs);
  f.view.remotePicker.path.set_value("/p/ch");
  byID(f.render(), "remote-folders").handlers.on_item_click("/p/chosen", f.cx);
  assert.equal(f.timers[0].cancelled, true); assert.equal(f.pending[1].request.path, "/p/chosen");
  f.timers[0].fire(); assert.equal(f.pending.length, 2);
  f.pending[1].resolve(snapshot("/p/chosen")); await Promise.all(f.jobs);
});
test("read errors retain the confirmed snapshot behind a separate retry layer", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/p", [entry("retained")])); await Promise.all(f.jobs);
  f.context.go(f.view, "/denied", f.cx); f.pending[1].reject(new Error("denied")); await Promise.all(f.jobs);
  const root = f.render(); assert.ok(byID(root, "remote-folder-retained")); assert.ok(byID(root, "remote-folder-error"));
  assert.ok(byID(root, "remote-folder-loading").attrs.invisible); assert.ok(byID(root, "remote-folder-retry"));
});
test("typing invalidates in-flight replies before debounce and reads stay serialized", async () => {
  const f = pickerFixture(); const p = f.view.remotePicker;
  p.path.set_value("/home/me/a"); f.timers[0].fire(); assert.equal(f.pending.length, 1);
  p.path.set_value("/home/me/b");
  f.pending[0].resolve(snapshot("/wrong")); await Promise.all(f.jobs);
  assert.equal(f.view.remoteBrowser.snapshot, null); assert.equal(f.pending.length, 1);
  f.timers[1].fire(); assert.equal(f.pending[1].request.query, "b");
  f.pending[1].resolve(snapshot("/home/me", [entry("beyond-page-one")])); await Promise.all(f.jobs);
  assert.equal(p.path.value(), "/home/me/b"); assert.equal(p.model.query, "b");
});
test("search during directory navigation targets the intended directory, not the old snapshot", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/old")); await Promise.all(f.jobs);
  f.context.go(f.view, "/new", f.cx); f.view.remotePicker.path.set_value("/new/target"); f.timers[0].fire();
  f.pending[1].resolve(snapshot("/new")); await tick();
  assert.equal(f.pending[2].request.path, "/new"); assert.equal(f.pending[2].request.query, "target");
  f.pending[2].resolve(snapshot("/new", [entry("target")])); await Promise.all(f.jobs);
  assert.equal(f.view.remotePicker.path.value(), "/new/target");
});
test("hidden preserves search, paging carries it, and navigation clears it", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/p")); await Promise.all(f.jobs);
  f.view.remotePicker.path.set_value("/p/pr"); f.timers[0].fire();
  f.pending[1].resolve({ ...snapshot("/p", [entry("project")]), nextCursor: "project" }); await Promise.all(f.jobs);
  byID(f.render(), "remote-folders").handlers.on_item_click("more", f.cx);
  assert.equal(f.pending[2].request.query, "pr"); assert.equal(f.pending[2].request.cursor, "project");
  f.pending[2].resolve(snapshot("/p", [entry("prototype")])); await Promise.all(f.jobs);
  assert.equal(f.view.remoteBrowser.snapshot.entries.length, 2);
  byID(f.render(), "remote-folder-hidden").handlers.on_click({}, f.cx);
  assert.equal(f.pending[3].request.query, "pr"); assert.equal(f.pending[3].request.hidden, true);
  f.pending[3].resolve(snapshot("/p", [entry("project")])); await Promise.all(f.jobs);
  byID(f.render(), "remote-folders").handlers.on_item_click("/p/project", f.cx);
  assert.equal(f.pending[4].request.query, ""); assert.equal(f.view.remotePicker.path.value(), "/p/project");
  f.pending[4].resolve(snapshot("/p/project")); await Promise.all(f.jobs);
});
test("type-to-filter starts from list keyboard and pending search tasks die on cancel", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/p")); await Promise.all(f.jobs);
  byID(f.render(), "remote-folder-list").handlers.on_key_down({ key: "p", key_char: "p" }, f.cx);
  assert.equal(f.view.remotePicker.path.value(), "/p/p");
  const address = f.view.remotePicker.path;
  f.context.cancel(f.view, f.cx); assert.equal(address.released, true); assert.equal(f.timers[0].cancelled, true);
  f.timers[0].fire(); assert.equal(f.pending.length, 1);
});
test("remote selection never replaces an explicit project name and controls are labelled icons", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/p")); await Promise.all(f.jobs);
  const root = f.render();
  for (const id of ["home", "up", "hidden"]) {
    const button = byID(root, `remote-folder-${id}`);
    assert.ok(button.attrs.accessibility_label[0]); assert.equal(typeof button.childNodes[0], "object");
  }
  f.view.name.set_value("My name"); f.context.choose(f.view, f.cx);
  f.pending[1].resolve(snapshot("/p")); await Promise.all(f.jobs);
  assert.equal(f.view.name.value(), "My name");
});

test("/home/user/PRO filters that parent and clicking a result enters its exact logical path", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/home/user", [entry("projects"), entry("bin")])); await Promise.all(f.jobs);
  const p = f.view.remotePicker;
  p.path.set_value("/home/user/PRO"); f.timers[0].fire();
  assert.equal(f.pending[1].request.path, "/home/user"); assert.equal(f.pending[1].request.query, "PRO");
  f.pending[1].resolve(snapshot("/home/user", [entry("projects")])); await Promise.all(f.jobs);
  assert.equal(p.path.value(), "/home/user/PRO");
  assert.equal(byID(f.render(), "remote-folder-choose").attrs.disabled[0], true);
  byID(f.render(), "remote-folders").handlers.on_item_click("/home/user/projects", f.cx);
  assert.equal(f.pending[2].request.path, "/home/user/projects"); assert.equal(f.pending[2].request.query, "");
  f.pending[2].resolve({ ...snapshot("/home/user/projects"), realPath: "/mnt/data/home/user/projects" }); await Promise.all(f.jobs);
  assert.equal(p.path.value(), "/home/user/projects");
  assert.equal(byID(f.render(), "remote-folder-choose").attrs.disabled[0], false);
});
test("trailing slash lists all children and allows choosing the confirmed parent", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/home/user")); await Promise.all(f.jobs);
  f.view.remotePicker.path.set_value("/home/user/"); f.timers[0].fire();
  assert.equal(f.pending[1].request.query, ""); assert.equal(f.pending[1].request.path, "/home/user");
  f.pending[1].resolve(snapshot("/home/user")); await Promise.all(f.jobs);
  f.context.choose(f.view, f.cx); assert.equal(f.pending[2].request.validate, true);
  f.pending[2].resolve(snapshot("/home/user")); await Promise.all(f.jobs);
  assert.equal(f.view.folder.value(), "/home/user"); assert.equal(f.view.name.value(), "user");
});
test("tilde address uses the remote home and backspacing invalidates an obsolete filtered reply", async () => {
  const f = pickerFixture(); f.pending[0].resolve(snapshot("/home/user")); await Promise.all(f.jobs);
  const p = f.view.remotePicker;
  p.path.set_value("~/projects/PRO"); f.timers[0].fire();
  assert.equal(f.pending[1].request.path, "~/projects"); assert.equal(f.pending[1].request.query, "PRO");
  p.path.set_value("~/projects/P"); f.pending[1].resolve(snapshot("/home/user/projects", [entry("projects")])); await Promise.all(f.jobs);
  assert.equal(f.view.remoteBrowser.snapshot.path, "/home/user");
  f.timers[1].fire(); assert.equal(f.pending[2].request.query, "P");
  f.pending[2].resolve(snapshot("/home/user/projects", [entry("packages")])); await Promise.all(f.jobs);
  assert.equal(p.path.value(), "~/projects/P");
});
test("Projects UI uses directory terminology without a second search field", () => {
  for (const file of ["form.js", "remote-picker.js", "remote-directory.js", "main.js"]) assert.doesNotMatch(source(file), /папк/i);
  assert.doesNotMatch(source("remote-picker.js"), /Найти подпапку|Input\.new\(p\.search\)/);
});
