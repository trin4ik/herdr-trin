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
  .replace(/^import .*;\n/gm, "").replaceAll("export function", "function").replace("export default class", "class");

function fixture() {
  const spaces = [{ id: "s", name: "Herdr", active: true, tabs: [{ id: "tab", named: true, name: "Named session" }] }];
  const agents = ["working", "idle", "blocked", "done"].map((status, seq) => ({
    address: `t${seq}`, pane: `p${seq}`, space: "s", tab: "tab", status, seq,
    state: "live", focused: seq === 1, program: { name: "OpenCode" },
  }));
  const commands = [], stored = {};
  const t = { accent: "#8ab4f8", chrome: "#101112", selected: "#25272b", muted: "#999999", dim: "#777777", idle: "#777777", working: "#d9bc65", blocked: "#e09080", done: "#8cb383" };
  const context = { View: class {}, div: element, svg: element, h_flex: element, v_flex: element,
    theme: () => t, props: () => ({}), layout: () => ({ id: "sidebar", size: { px: 44 } }),
    spaces: () => spaces, agents: () => agents, terminals: () => agents,
    shared: key => stored[key], setShared: (key, value) => { stored[key] = value; },
    focused: () => ({ agent: "t1" }), setAgentOrder: order => { context.order = order; },
    window: () => ({ platform: "macos" }),
    localStorage: { getItem: () => null, setItem() {} },
    run: (name, args) => commands.push({ name, args }),
  };
  vm.runInNewContext(read("trin-agents/appearance.js") + "\n" + read("trin-agents/main.js")
    + "\nthis.AgentsPanel = Agents;", context);
  const panel = new context.AgentsPanel();
  Object.assign(panel, { pins: [], changed: {}, folded: new Set(), clock: { sync() {} },
    activity: { details: () => ({ providerID: "openai", modelID: "gpt-test", variant: "medium" }), notice: () => null } });
  return { panel, commands, agents, context, t, spaces };
}

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
    row.handlers.on_click();
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
  byID(root, "collapse").handlers.on_click();
  assert.equal(commands.at(-2).args.id, "sidebar");
  assert.equal(commands.at(-2).args.px, 268);
  assert.equal(commands.at(-1).args.id, "quotas");
  assert.equal(commands.at(-1).args.px, 152);
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
