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
  for (const [id, type] of Object.entries({ sidebar: "column", "sidebar-lists": "column", "work-body": "row", panel: "column", "panel-body": "stack" })) {
    assert.equal(list.find(n => n.id === id)?.type, type, id);
  }
  assert.deepEqual(list.find(n => n.id === "panel-body").children.map(n => n.id), ["files", "changes"]);
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

test("compact quota block sits outside the Spaces/Agents split, immediately above Settings", () => {
  const sidebar = nodes(layout).find(n => n.id === "sidebar");
  const ids = sidebar.children.map(n => n.id);
  assert.deepEqual(ids.slice(-3), ["sidebar-lists", "quotas", "sidebar-footer"]);
  const quota = sidebar.children.find(n => n.id === "quotas");
  assert.equal(quota.size.px, 180, "header and five subscription rows fit without scrolling in the wide sidebar");
  assert.equal(quota.content.mod, "trin-quotas");
});

test("only the brand header uses the custom clone; Settings remains built-in", () => {
  const list = nodes(layout), brand = list.find(n => n.id === "brand");
  assert.equal(brand.content.mod, "trin-brand");
  assert.equal(brand.window_drag, true);
  assert.equal(brand.size.px, 46);
  assert.equal(list.find(n => n.id === "sidebar-footer").content.mod, "workspace-chrome");
});
