import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

// Native builder stand-in: execute the real view handlers, not a second copy
// of drag logic. This does not pretend to verify GPUI geometry or rendering.
function element() {
  const state = { childNodes: [], handlers: {}, attributes: {} };
  const builder = new Proxy(state, {
    get(target, key) {
      if (key in target) return target[key];
      return (...args) => {
        if (key === "when") { if (args[0]) args[1](builder); }
        else if (key === "child") state.childNodes.push(args[0]);
        else if (key === "children") state.childNodes.push(...args[0]);
        else if (key === "on_mouse_down" || key === "on_mouse_up") state.handlers[`${key}:${args[0]}`] = args[1];
        else if (key.startsWith("on_")) state.handlers[key] = args[0];
        else state.attributes[key] = args;
        return builder;
      };
    },
  });
  return builder;
}

function find(node, id) {
  if (!node || typeof node !== "object") return null;
  if (node.attributes.id?.[0] === id) return node;
  for (const child of node.childNodes) {
    const result = find(child, id);
    if (result) return result;
  }
  return null;
}

function fixture(mod, initial, platform = "macos") {
  let list = initial, tabPanes = [], live = [];
  const sharedState = {};
  const commands = [];
  const source = fs.readFileSync(new URL(`../mods/${mod}/main.js`, import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace("export default class", "class");
  const name = mod === "trin-tabs" ? "Tabs" : "Spaces";
  const context = {
    View: class {}, div: element, svg: element, image: element, h_flex: element, v_flex: element,
    spaces: () => list, panes: () => tabPanes, terminals: () => live, shared: key => sharedState[key], theme: () => ({}),
    window: () => ({ platform }), layout: () => ({}), settings: () => ({ zoom: 1 }),
    run: (command, args) => commands.push({ command, args }),
    localStorage: { getItem: () => null, setItem() {} },
  };
  vm.runInNewContext(source + `\nthis.Panel = ${name};`, context);
  const panel = new context.Panel();
  Object.assign(panel, { pressed: null, over: null, panels: {}, spaceAgents: new Map() });
  return { panel, commands, sharedState, setList: next => { list = next; },
    setTerminals: (next, panes) => { live = next; tabPanes = panes; } };
}

const cx = { stop_propagation() {}, notify() {} };
const tab = id => ({ id, name: id, active: id === "a" });
const space = (id, machine = null) => ({ id, name: id, machine });

test("drag preview shifts neighbours in both directions and commits only on release", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b"), tab("c")] }]);
  find(f.panel.render(), "tab-a").handlers["on_mouse_down:left"]({ click_count: 1, position: { x: 20 }, bounds: { width: 110 } }, cx);
  f.panel.moveDrag({ position: { x: 150 } }, cx);
  assert.equal(f.panel.drag.to, 1);
  assert.equal(find(f.panel.render(), "tab-b").attributes.left[0], -110);
  assert.equal(find(f.panel.render(), "tab-a").attributes.opacity[0], 0);
  assert.ok(find(f.panel.render(), "tab-drag-ghost"));
  assert.ok(find(f.panel.render(), "tab-drag-ghost").attributes.shadow_md);
  assert.equal(find(f.panel.render(), "tab-drag-ghost").attributes.shadow, undefined);
  assert.equal(f.commands.length, 0);
  f.panel.moveDrag({ position: { x: 25 } }, cx);
  assert.equal(f.panel.drag.to, 0);
  assert.equal(find(f.panel.render(), "tab-b").attributes.left[0], 0);
  f.panel.moveDrag({ position: { x: 260 } }, cx);
  find(f.panel.render(), "tabs-root").handlers["on_mouse_up:left"]({}, cx);
  assert.equal(f.commands[0].command, "tab.move");
  assert.equal(f.commands[0].args.id, "a");
  assert.equal(f.commands[0].args.index, 3);
});

test("closing a tab during a drag cancels rather than moving its replacement", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b")] }]);
  find(f.panel.render(), "tab-a").handlers["on_mouse_down:left"]({ click_count: 1, position: { x: 20 }, bounds: { width: 110 } }, cx);
  f.setList([{ id: "space", active: true, tabs: [tab("b")] }]);
  find(f.panel.render(), "tabs-root").handlers["on_mouse_up:left"]({}, cx);
  assert.equal(f.commands.length, 0);
});

test("click selects, double-click renames; leaving the strip cancels without reordering", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b")] }]);
  const down = click_count => find(f.panel.render(), "tab-a").handlers["on_mouse_down:left"]({ click_count, position: { x: 20 }, bounds: { width: 110 } }, cx);
  down(1); f.panel.finishDrag(cx);
  assert.equal(f.commands.at(-1).command, "tab.select");
  down(2); assert.equal(f.commands.at(-1).command, "tab.rename");
  const count = f.commands.length;
  down(1); f.panel.moveDrag({ position: { x: 180 } }, cx);
  find(f.panel.render(), "tabs-root").handlers.on_hover(false, cx);
  f.panel.finishDrag(cx);
  assert.equal(f.panel.drag, null); assert.equal(f.commands.length, count);
});

test("leftward drag uses measured widths and returns neighbours when reversed", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b"), tab("c")] }]);
  const root = f.panel.render();
  for (const [id, width] of [["a", 140], ["b", 200], ["c", 110]]) find(root, `tab-${id}`).handlers.on_size_change({ width }, cx);
  find(root, "tab-c").handlers["on_mouse_down:left"]({ click_count: 1, position: { x: 370 }, bounds: { width: 110 } }, cx);
  f.panel.moveDrag({ position: { x: 80 } }, cx);
  assert.equal(f.panel.drag.to, 1); assert.equal(f.panel.offset("b"), 110);
  f.panel.moveDrag({ position: { x: 0 } }, cx);
  assert.equal(f.panel.drag.to, 0); assert.equal(f.panel.offset("a"), 110);
  f.panel.moveDrag({ position: { x: 370 } }, cx);
  assert.equal(f.panel.drag.to, 2); assert.equal(f.panel.offset("b"), 0);
  f.panel.finishDrag(cx); assert.equal(f.commands.length, 0);
});

test("external reorders, viewport resize and project switches cancel an in-flight preview", () => {
  for (const change of ["reorder", "resize", "project"]) {
    const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b")] }]);
    let root = f.panel.render();
    find(root, "tabs-root").handlers.on_size_change({ width: 900 }, cx);
    find(root, "tab-a").handlers.on_size_change({ width: 110 }, cx);
    find(root, "tab-a").handlers["on_mouse_down:left"]({ click_count: 1, position: { x: 20 }, bounds: { width: 110 } }, cx);
    f.panel.moveDrag({ position: { x: 180 } }, cx);
    if (change === "resize") find(root, "tabs-root").handlers.on_size_change({ width: 600 }, cx);
    else f.setList([{ id: change === "project" ? "other" : "space", active: true, tabs: [tab("b"), tab("a")] }]);
    root = f.panel.render();
    assert.equal(find(root, "tab-drag-ghost"), null);
    f.panel.finishDrag(cx); assert.equal(f.commands.length, 0);
  }
});

test("loading-agent title changes freeze widths without cancelling drag, including before its threshold", () => {
  for (const move of [false, true]) {
    const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b")] }]);
    let root = f.panel.render();
    for (const id of ["a", "b"]) find(root, `tab-${id}`).handlers.on_size_change({ width: 160 }, cx);
    find(root, "tab-b").handlers["on_mouse_down:left"]({ click_count: 1, position: { x: 190 }, bounds: { width: 160 } }, cx);
    if (move) f.panel.moveDrag({ position: { x: 10 } }, cx);
    const before = f.panel.drag;
    f.setList([{ id: "space", active: true, tabs: [{ ...tab("a"), name: "Neighbour now has a much longer title", named: true }, { ...tab("b"), name: "OpenCode — session title after startup", named: true }] }]);
    root = f.panel.render();
    for (const id of ["a", "b"]) {
      assert.equal(find(root, `tab-${id}`).attributes.w[0], 160);
      find(root, `tab-${id}`).handlers.on_size_change({ width: 240 }, cx);
      assert.equal(f.panel.widths.get(id), 160);
    }
    assert.equal(f.panel.drag, before);
    if (!move) f.panel.moveDrag({ position: { x: 10 } }, cx);
    f.panel.finishDrag(cx);
    assert.equal(f.commands.at(-1).command, "tab.move");
    assert.equal(f.commands.at(-1).args.id, "b");
    assert.equal(f.commands.at(-1).args.index, 1);
    root = f.panel.render();
    assert.equal(find(root, "tab-b").attributes.w, undefined);
    find(root, "tab-b").handlers.on_size_change({ width: 240 }, cx);
    assert.equal(f.panel.widths.get("b"), 240);
  }
});

test("each tab contains only title and close, including inactive and working tabs", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [{ ...tab("a"), status: "working" }, tab("b")] }]);
  for (const id of ["a", "b"]) {
    const node = find(f.panel.render(), `tab-${id}`);
    // Active top-edge highlight is decoration; no program logo or status spinner.
    assert.ok(find(node, `close-${id}`));
    assert.equal(node.childNodes.length, id === "a" ? 3 : 2);
  }
});

test("tabs display the verified OpenCode title even if named, and do not reuse another session's name", () => {
  const initial = [{ id: "space", active: true, tabs: [tab("a")] }];
  const f = fixture("trin-tabs", initial);
  const report = { app: "opencode", state: "working", title: "Terminal name…" };
  const terminal = { address: "t1", report, records: { ses_live: report } };
  f.setTerminals([terminal], [{ tab: "a", terminal: "t1", title: "OC | Terminal name…" }]);
  const name = () => find(f.panel.render(), "tab-a").childNodes[1].childNodes[0];
  const setTitle = title => { f.sharedState["trin.sessions.titles"] = { t1: { sessionID: "ses_live", title } }; };
  setTitle("Full server name");
  assert.equal(name(), "Full server name");
  setTitle("Renamed on the server");
  assert.equal(name(), "Renamed on the server");
  terminal.records = { ses_other: report };
  assert.equal(name(), "Terminal name…");
  terminal.records = { ses_live: report };
  f.setList([{ ...initial[0], tabs: [{ ...tab("a"), named: true, name: "User alias" }] }]);
  assert.equal(name(), "Renamed on the server");
  terminal.records = {};
  terminal.report = { app: "opencode", state: "idle" };
  Object.assign(f.sharedState["trin.sessions.titles"].t1, { pid: 12, created: 34, caption: "OC | Terminal name…" });
  terminal.pid = 12;
  terminal.created_ms = 34;
  assert.equal(name(), "Renamed on the server");
  terminal.pid = 13;
  assert.equal(name(), "User alias");
  assert.deepEqual(f.commands, []);
});

test("OpenCode button uses the same project-root action as Command-T", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a")] }]);
  const root = f.panel.render(), button = find(root, "tab-new");
  assert.equal(button.attributes.h[0], 26);
  assert.equal(button.childNodes[0].attributes.w[0], 8);
  assert.equal(button.childNodes[0].attributes.h[0], 10);
  assert.equal(button.childNodes[0].attributes.opacity[0], 0.75);
  assert.equal(find(root, "tab-new-shortcut").childNodes[0], "⌘ T");
  assert.equal(find(root, "tab-new-shortcut").attributes.opacity[0], 0.65);
  button.handlers.on_click();
  assert.equal(f.commands.length, 1);
  assert.equal(f.commands[0].command, "trin-projects.new-tab");
});

test("separate terminal button launches a shell and advertises Shift-Command-T", () => {
  const f = fixture("trin-tabs", []), root = f.panel.render();
  assert.equal(find(root, "tab-terminal-shortcut").childNodes[0], "⇧ ⌘ T");
  assert.equal(find(root, "tab-terminal").childNodes[0].attributes.size[0], 12);
  assert.equal(find(root, "tab-terminal").childNodes[0].attributes.opacity[0], 0.75);
  find(root, "tab-terminal").handlers.on_click();
  assert.equal(f.commands[0].command, "trin-projects.new-terminal");
  assert.equal(find(find(root, "tab-scroll"), "tab-terminal"), null);
  assert.equal(find(root, "tab-create").attributes.gap[0], 14);
});

test("New and shortcut stay outside the scrollable tab list, including an empty space", () => {
  for (const tabs of [[], [tab("a"), tab("b"), tab("c")]]) {
    const f = fixture("trin-tabs", [{ id: "space", active: true, tabs }]);
    const root = f.panel.render(), strip = find(root, "tab-strip"), scroll = find(root, "tab-scroll");
    assert.ok(strip.attributes.flex_1);
    assert.equal(scroll.attributes.flex_1, undefined);
    assert.equal(scroll.attributes.w[0], "auto");
    assert.ok(scroll.attributes.flex_none);
    assert.equal(scroll.attributes.flex_shrink[0], 1);
    assert.equal(find(scroll, "tab-new"), null);
    assert.ok(find(strip, "tab-create").attributes.flex_none);
    assert.ok(find(root, "tab-new"));
  }
});

test("non-macOS tab strip uses a Ctrl shortcut label", () => {
  const f = fixture("trin-tabs", [], "windows");
  assert.equal(find(f.panel.render(), "tab-new-shortcut").childNodes[0], "Ctrl T");
});

test("Files/Changes preference survives visiting a project with no native Space", () => {
  const f = fixture("trin-tabs", []), panel = { hidden: false }, body = { active: "files" };
  f.panel.syncPanel({ id: "project-a" }, panel, body, ["files", "changes"]);
  assert.equal(f.panel.panels["project-a"], "files");
  panel.hidden = true; // Projects forces it hidden, not a user preference change.
  f.panel.syncPanel({ id: "empty-b" }, panel, body, ["files", "changes"]);
  f.panel.syncPanel({ id: "project-a" }, panel, body, ["files", "changes"]);
  assert.equal(f.panel.panels["project-a"], "files");
  assert.equal(f.commands.at(-1).command, "layout.show");
});

test("space drag resolves the source and destination by current ID", () => {
  const a = space("a"), b = space("b"), c = space("c");
  const f = fixture("trin-spaces", [a, b, c]);
  f.panel.row({ w: a, i: 0 }, {}).handlers["on_mouse_down:left"]();
  f.setList([c, a, b]);
  const destination = f.panel.row({ w: b, i: 2 }, {});
  destination.handlers.on_mouse_move({}, cx);
  destination.handlers["on_mouse_up:left"]({}, cx);
  assert.equal(f.commands[0].command, "space.move");
  assert.equal(f.commands[0].args.id, "a");
  assert.equal(f.commands[0].args.index, 3);
});

test("a vanished space cannot cause an unrelated space to move", () => {
  const a = space("a"), b = space("b");
  const f = fixture("trin-spaces", [a, b]);
  f.panel.row({ w: a, i: 0 }, {}).handlers["on_mouse_down:left"]();
  f.setList([b]);
  const destination = f.panel.row({ w: b, i: 0 }, {});
  destination.handlers.on_mouse_move({}, cx);
  destination.handlers["on_mouse_up:left"]({}, cx);
  assert.equal(f.commands.length, 0);
});

test("space dragging never crosses machine boundaries", () => {
  const a = space("a"), b = space("b", "remote");
  const f = fixture("trin-spaces", [a, b]);
  f.panel.row({ w: a, i: 0 }, {}).handlers["on_mouse_down:left"]();
  const destination = f.panel.row({ w: b, i: 1 }, {});
  destination.handlers.on_mouse_move({}, cx);
  destination.handlers["on_mouse_up:left"]({}, cx);
  assert.equal(f.commands.length, 0);
});

test("an ordinary space click still selects its ID", () => {
  const a = space("a");
  const f = fixture("trin-spaces", [a]);
  const row = f.panel.row({ w: a, i: 0 }, {});
  row.handlers["on_mouse_down:left"]();
  row.handlers["on_mouse_up:left"]({}, cx);
  assert.equal(f.commands[0].command, "space.select");
  assert.equal(f.commands[0].args.id, "a");
});
