import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const root = new URL("../", import.meta.url);
const layout = JSON.parse(fs.readFileSync(new URL("layouts/trin.json", root), "utf8"));

function nodes(node) {
  return [node, ...(node.children ?? []).flatMap(nodes)];
}

function mods(value) {
  if (!value || typeof value !== "object") return [];
  return [...(typeof value.mod === "string" ? [value.mod] : []), ...Object.values(value).flatMap(mods)];
}

test("layout node IDs are unique and preserve the commands' structural targets", () => {
  const list = nodes(layout);
  const ids = list.map(n => n.id);
  assert.equal(new Set(ids).size, ids.length);
  for (const [id, type] of Object.entries({ sidebar: "column", "sidebar-lists": "column", "session-body": "stack", "work-body": "row", panel: "column", "panel-body": "stack" })) {
    assert.equal(list.find(n => n.id === id)?.type, type, id);
  }
  assert.deepEqual(list.find(n => n.id === "panel-body").children.map(n => n.id), ["files", "changes"]);
  const sessionBody = list.find(n => n.id === "session-body");
  assert.equal(sessionBody.active, "work-body");
  assert.deepEqual(sessionBody.children.map(n => n.id), ["work-body", "hidden-tabs-empty"]);
});

test("layout uses implemented custom mods and explicitly known built-ins", () => {
  const custom = new Set(["trin-agents", "trin-projects", "trin-tabs", "trin-quotas", "trin-brand"]);
  const builtins = new Set(["workspace-chrome", "pane", "files", "changes"]);
  const used = new Set(mods(layout));
  for (const name of used) {
    if (builtins.has(name)) continue;
    assert.ok(custom.has(name), `unexpected mod dependency: ${name}`);
    assert.ok(fs.existsSync(new URL(`mods/${name}/main.js`, root)), `${name} needs an implementation, not just generated types`);
    JSON.parse(fs.readFileSync(new URL(`mods/${name}/mod.json`, root), "utf8"));
  }
  for (const name of custom) assert.ok(used.has(name), `${name} is missing from the layout`);
});

test("native quota divider moves independently of the Spaces/Agents split and fixed chrome", () => {
  const sidebar = nodes(layout).find(n => n.id === "sidebar");
  assert.deepEqual(sidebar.children.map(n => n.id), ["brand", "sidebar-body", "sidebar-footer"]);
  assert.equal(sidebar.resizable, false);
  const body = sidebar.children[1];
  assert.equal(body.type, "column");
  assert.equal(body.resizable, true);
  assert.deepEqual(body.children.map(n => n.id), ["sidebar-lists", "quotas"]);
  const [lists, quota] = body.children;
  assert.deepEqual(lists.children.map(n => n.id), ["spaces", "agents"]);
  assert.notEqual(lists.resizable, false);
  assert.equal(lists.size?.px, undefined, "only quotas use fixed pixels in the split");
  assert.ok(lists.min >= lists.children.reduce((sum, n) => sum + n.min, 0));
  assert.ok(quota.min > 0 && quota.min < quota.size.px);
  assert.equal(quota.size.px, 180, "block fits five quota subscriptions including OpenCode Go");
  assert.equal(quota.content.mod, "trin-quotas");
});

test("only the brand header uses the custom clone; Settings remains built-in", () => {
  const list = nodes(layout), brand = list.find(n => n.id === "brand");
  assert.equal(brand.content.mod, "trin-brand");
  assert.equal(brand.window_drag, true);
  assert.equal(brand.size.px, 46);
  assert.equal(list.find(n => n.id === "sidebar-footer").content.mod, "workspace-chrome");
});
