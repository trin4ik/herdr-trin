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
  let list = initial;
  const commands = [];
  const source = fs.readFileSync(new URL(`../mods/${mod}/main.js`, import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace("export default class", "class");
  const name = mod === "trin-tabs" ? "Tabs" : "Spaces";
  const context = {
    View: class {}, div: element, svg: element, image: element, h_flex: element, v_flex: element,
    spaces: () => list, panes: () => [], shared: () => undefined, theme: () => ({}),
    window: () => ({ platform }), layout: () => ({}), settings: () => ({ zoom: 1 }),
    run: (command, args) => commands.push({ command, args }),
    localStorage: { getItem: () => null, setItem() {} },
  };
  vm.runInNewContext(source + `\nthis.Panel = ${name};`, context);
  const panel = new context.Panel();
  Object.assign(panel, { pressed: null, over: null, panels: {}, spaceAgents: new Map() });
  return { panel, commands, setList: next => { list = next; } };
}

const cx = { stop_propagation() {}, notify() {} };
const tab = id => ({ id, name: id, active: id === "a" });
const space = (id, machine = null) => ({ id, name: id, machine });

test("tab drag resolves current IDs and target index after the list changes", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b"), tab("c")] }]);
  find(f.panel.render(), "tab-a").handlers["on_mouse_down:left"]({ click_count: 1 }, cx);
  f.setList([{ id: "space", active: true, tabs: [tab("c"), tab("a"), tab("b")] }]);
  find(f.panel.render(), "tab-b").handlers["on_mouse_up:left"]();
  assert.equal(f.commands[0].command, "tab.move");
  assert.equal(f.commands[0].args.id, "a");
  assert.equal(f.commands[0].args.index, 3);
});

test("closing a tab during a drag cancels rather than moving its replacement", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a"), tab("b")] }]);
  find(f.panel.render(), "tab-a").handlers["on_mouse_down:left"]({ click_count: 1 }, cx);
  f.setList([{ id: "space", active: true, tabs: [tab("b")] }]);
  find(f.panel.render(), "tab-b").handlers["on_mouse_up:left"]();
  assert.equal(f.commands.length, 0);
});

test("square New button uses the same project-root action as Command-T", () => {
  const f = fixture("trin-tabs", [{ id: "space", active: true, tabs: [tab("a")] }]);
  const root = f.panel.render(), button = find(root, "tab-new");
  assert.equal(button.attributes.size[0], 18);
  assert.equal(button.childNodes[0].attributes.size[0], 14);
  assert.equal(button.attributes.border[0], 1);
  assert.equal(find(root, "tab-new-shortcut").childNodes[0], "⌘ T");
  assert.equal(find(root, "tab-new-shortcut").attributes.opacity[0], 0.65);
  button.handlers.on_click();
  assert.equal(f.commands.length, 1);
  assert.equal(f.commands[0].command, "trin-projects.new-tab");
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
