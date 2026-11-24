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

const mouse = (y, local = 4, height = 52) => ({
  position: { x: 12, y }, local_position: { x: 12, y: local }, bounds: { width: 200, height },
});
const cx = { notify() {}, stop_propagation() {} };
const click = row => {
  row.handlers.on_mouse_down(mouse(10), cx);
  row.handlers.on_mouse_up(mouse(10), cx);
};

function fixture(storage = {}) {
  const spaces = [{ id: "s", name: "Herdr", active: true, tabs: [{ id: "tab", named: true, name: "Named session" }] }];
  const agents = ["working", "idle", "blocked", "done"].map((status, seq) => ({
    address: `t${seq}`, pane: `p${seq}`, space: "s", tab: "tab", status, seq,
    state: "live", focused: seq === 1, program: { name: "OpenCode" },
  }));
  const commands = [], badges = [], stored = {};
  const t = { accent: "#8ab4f8", chrome: "#101112", selected: "#25272b", muted: "#999999", dim: "#777777", idle: "#777777", working: "#d9bc65", blocked: "#e09080", done: "#8cb383" };
  const context = { View: class {}, div: element, svg: path => element().asset(path), h_flex: element, v_flex: element,
    formatTokens: () => "", formatActiveTime: () => "", modelLabel: () => "model", contextTooltip: () => "",
    theme: () => t, layout: () => ({ id: "sidebar", size: { px: 44 } }),
    spaces: () => spaces, agents: () => agents, terminals: () => agents,
    shared: key => stored[key], setShared: (key, value) => { stored[key] = value; },
    focused: () => ({ agent: "t1" }), setAgentOrder: order => { context.order = order; },
    setBadge: text => badges.push(text),
    window: () => ({ platform: "macos" }),
    localStorage: { getItem: key => storage[key] ?? null, setItem(key, value) { storage[key] = value; } },
    run: (name, args) => commands.push({ name, args }),
  };
  vm.runInNewContext(read("trin-agents/order.js") + "\n" + read("trin-agents/appearance.js") + "\n" + read("trin-agents/main.js")
    + "\nthis.AgentsPanel = Agents;", context);
  context.sessionAddress = (machine, id) => `opencode-session:${encodeURIComponent(machine ?? "")}/${id}`;
  const panel = new context.AgentsPanel();
  Object.assign(panel, { order: JSON.parse(storage["sessions.order"] ?? storage.pins ?? "[]"), clock: { sync() {}, value: () => null },
    activity: { details: () => ({ providerID: "openai", modelID: "gpt-test", variant: "medium" }), get: () => null, notice: () => null, close() {} } });
  return { panel, commands, badges, agents, context, t, spaces, storage };
}

test("Dock badge sums live blocked and unseen done sessions reactively across projects", () => {
  const { panel, agents, badges, context } = fixture();
  agents[3].space = "remote/other-project";
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

test("wide Sessions tints only unread results blue and retains the status dot", () => {
  const { panel, agents, context, t } = fixture();
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  const root = panel.render();
  const done = byID(root, "ag-t3");
  assert.equal(done.attrs.bg[0], "#3b82f620");
  const hovered = done.attrs.hover[0](element());
  assert.equal(hovered.attrs.bg[0], "#3b82f62b");
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

test("background sessions are visible and read-only, with no terminal navigation entries", () => {
  const { panel, commands, agents, context } = fixture();
  panel.activity.backgroundRows = () => [{ address: "opencode-session:/ses_job", background: true,
    title: "Background job", folder: "/fixture", program: { name: "OpenCode" }, status: "working" }];
  panel.activity.session = a => a === agents[1] ? { excluded: true } : null;
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
  assert.deepEqual(commands, []);
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
  const { panel, agents, storage, commands } = fixture();
  const detached = { address: "opencode-session:/ses_job", sessionID: "ses_job", background: true,
    title: "Background job", folder: "/fixture", program: { name: "OpenCode" }, status: "working" };
  let running = true;
  panel.activity.backgroundRows = () => running ? [detached] : [];
  const row = id => byID(panel.render(), `ag-${id}`);
  row(detached.address).handlers.on_mouse_down(mouse(10), cx);
  row("t0").handlers.on_mouse_move(mouse(70), cx);
  row("t0").handlers.on_mouse_up(mouse(70), cx);
  assert.equal(sessionRows(panel)[0], detached.address);
  assert.equal(JSON.parse(storage["sessions.order"])[0], detached.address);
  running = false;
  assert.equal(sessionRows(panel)[0], "t0");
  const attached = { ...agents[0], address: "t-resumed" };
  agents.push(attached);
  panel.activity.session = a => a === attached ? { sessionID: "ses_job" } : null;
  assert.equal(sessionRows(panel)[0], attached.address);
  panel.activity.session = () => null;
  assert.equal(sessionRows(panel)[0], attached.address, "transient rename gap does not change manual rank");
  assert.deepEqual(commands, []);
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

test("Agents falls back to the existing provider assets or initial for unknown families", () => {
  const context = { div: element, svg: path => element().asset(path), h_flex: element,
    formatTokens: () => "", formatActiveTime: () => "", modelLabel: () => "model", contextTooltip: () => "" };
  vm.runInNewContext(read("trin-agents/appearance.js") + "\nthis.details = sessionDetails;", context);
  const quotaContext = { ...context, View: class {} };
  vm.runInNewContext(read("trin-quotas/main.js") + "\nthis.QuotasPanel = Quotas;", quotaContext);
  const quotaPanel = new quotaContext.QuotasPanel();
  for (const provider of ["openai", "anthropic", "ollama", "kimi", "opencode", "google"]) {
    const root = context.details({ muted: "muted" }, { providerID: provider }, null, null, "test");
    const icon = nodes(root).find(n => n.attrs.asset);
    const path = provider === "google" ? "mods/trin-agents/model-google.svg"
      : `assets/trin-providers/${provider === "opencode" ? "opencode-logo" : provider}.svg`;
    assert.equal(icon.attrs.asset[0], path);
    assert.equal(icon.attrs.text_color[0], "muted");
    const quotaIcon = nodes(quotaPanel.mark({ provider, name: provider }, { muted: "muted" })).find(n => n.attrs.asset);
    assert.equal(quotaIcon?.attrs.asset[0], path, provider);
    assert.equal(quotaIcon.attrs.text_color[0], "muted");
    if (provider === "opencode") {
      assert.equal(icon.attrs.w[0], 10); assert.equal(icon.attrs.h[0], 12.5);
    } else assert.equal(icon.attrs.size[0], ["kimi", "google"].includes(provider) ? 16 : 20);
  }
  const unknown = context.details({}, { providerID: "custom" }, null, null, "test");
  assert.equal(nodes(unknown).some(n => n.attrs.asset), false);
  assert.ok(text(unknown).startsWith("C"));
  for (const [provider, mark] of [["kimi-code-plan-cn", "kimi"], ["kimi-for-coding", "kimi"], ["ollama-cloud", "ollama"], ["opencode-go", "opencode"]]) {
    const root = context.details({ muted: "muted" }, { providerID: provider, modelID: "unknown-model" }, null, null, "test");
    const icon = nodes(root).find(n => n.attrs.asset);
    const path = `assets/trin-providers/${mark === "opencode" ? "opencode-logo" : mark}.svg`;
    assert.equal(icon.attrs.asset[0], path);
    if (mark === "opencode") {
      assert.equal(icon.attrs.w[0], 10);
      assert.equal(icon.attrs.h[0], 12.5);
    } else assert.equal(icon.attrs.size[0], mark === "kimi" ? 16 : 20);
    assert.ok(byID(root, "agent-test-model").attrs.tooltip[0].startsWith(provider + "/"));
    assert.equal(nodes(root).some(n => n.childNodes.includes(provider[0].toUpperCase())), false);
  }
});

test("Agents updates the family mark from the current model without changing provider metadata", () => {
  const { panel, context } = fixture();
  context.layout = () => ({ id: "sidebar", size: { px: 268 } });
  for (const [providerID, modelID, path] of [
    ["openrouter", "openai/gpt-5.5", "assets/trin-providers/openai.svg"],
    ["custom", "GPT 5.5", "assets/trin-providers/openai.svg"],
    ["opencode-go", "o4-mini", "assets/trin-providers/openai.svg"],
    ["custom", "claude-haiku-4-5", "assets/trin-providers/anthropic.svg"],
    ["openrouter", "anthropic/claude-sonnet-5", "assets/trin-providers/anthropic.svg"],
    ["openai", "Opus 5.5", "assets/trin-providers/anthropic.svg"],
    ["openrouter", "anthropic/claude-fable-5.2", "assets/trin-providers/anthropic.svg"],
    ["openrouter", "google/gemini-3.1-pro-preview:free", "mods/trin-agents/model-google.svg"],
    ["google", "gemini-3.1-flash", "mods/trin-agents/model-google.svg"],
    ["ollama", "gemma4:31b", "mods/trin-agents/model-google.svg"],
    ["ollama", "inkling-2-mini:26b-a5b", "mods/trin-agents/model-thinking-machines.svg"],
    ["local", "thinkingmachines/Inkling-2-Mini", "mods/trin-agents/model-thinking-machines.svg"],
    ["opencode-go", "moonshotai/kimi-k2.5", "assets/trin-providers/kimi.svg"],
    ["ollama", "gpt-tools/not-inkling", "assets/trin-providers/ollama.svg"],
    ["opencode-go", "fablewriter", "assets/trin-providers/opencode-logo.svg"],
    ["custom", "google/geminized", null],
    ["custom", undefined, null],
    [undefined, "gemini-3.1-pro", "mods/trin-agents/model-google.svg"],
  ]) {
    panel.activity.details = () => ({ providerID, modelID, variant: "medium" });
    const row = byID(panel.render(), "agent-t0-model");
    const icon = nodes(row).find(n => n.attrs.asset);
    assert.equal(icon?.attrs.asset[0] ?? null, path, `${providerID}/${modelID}`);
    assert.equal(row.attrs.tooltip[0], modelID ? `${providerID ?? "?"}/${modelID} · medium` : "Model data unavailable");
    if (path?.startsWith("mods/")) {
      assert.equal(icon.attrs.size[0], 16);
      assert.ok(fs.existsSync(new URL(`../${path}`, import.meta.url)), path);
    }
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
