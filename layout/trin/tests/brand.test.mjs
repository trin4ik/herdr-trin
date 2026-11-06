import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

function element(kind = "div", path = null) {
  const state = { kind, path, childNodes: [], attrs: {}, handlers: {} };
  const builder = new Proxy(state, { get(target, key) {
    if (key in target) return target[key];
    return (...args) => {
      if (key === "child") state.childNodes.push(args[0]);
      else if (key === "when") { if (args[0]) args[1](builder); }
      else if (key === "on_click") state.handlers.click = args[0];
      else state.attrs[key] = args;
      return builder;
    };
  } });
  return builder;
}
function nodes(node) { return node && typeof node === "object" ? [node, ...node.childNodes.flatMap(nodes)] : []; }
function text(node) { return node && typeof node === "object" ? node.childNodes.map(text).join("") : String(node ?? ""); }
function fixture(width = 268, platform = "macos") {
  const commands = [], stored = {};
  const source = fs.readFileSync(new URL("../mods/trin-brand/main.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace("export default class", "class");
  const context = { View: class {}, div: () => element(), h_flex: () => element(), v_flex: () => element(),
    image: path => element("image", path), svg: path => element("svg", path),
    theme: () => ({}), props: () => ({}), window: () => ({ platform, fullscreen: false }),
    layout: () => ({ id: "sidebar", size: { px: width } }),
    shared: key => stored[key], setShared: (key, value) => { stored[key] = value; },
    run: (name, args) => commands.push({ name, args }) };
  vm.runInNewContext(source + "\nthis.Panel = WorkspaceChrome;", context);
  return { panel: new context.Panel(), commands, stored };
}

test("brandbook SVG is byte-identical and has not been recolored", () => {
  const bytes = fs.readFileSync(new URL("../mods/trin-brand/deleeva-mark-dark.svg", import.meta.url));
  assert.equal(crypto.createHash("sha256").update(bytes).digest("hex"), "4a4f6a8472bad151f3f543b8fbdf3adf3ee22df3900b68c1c55aad39f8b51d8d");
});
test("runtime raster preserves the canonical mark's aspect ratio at high density", () => {
  const png = fs.readFileSync(new URL("../mods/trin-brand/deleeva-mark-dark.png", import.meta.url));
  assert.equal(png.subarray(0, 8).toString("hex"), "89504e470d0a1a0a");
  assert.equal(png.readUInt32BE(16), 192);
  assert.equal(png.readUInt32BE(20), 204);
});
test("header says Deliva and preserves full-color image proportions", () => {
  const { panel } = fixture(), root = panel.render();
  assert.ok(text(root).includes("Deliva"));
  assert.equal(text(root).includes("Herdr"), false);
  const mark = nodes(root).find(n => n.kind === "image");
  assert.equal(mark.path, "assets/trin-brand/deleeva-mark-dark.png");
  assert.equal(mark.attrs.w[0], 20);
  assert.equal(mark.attrs.h[0], 21.25);
  assert.equal(mark.attrs.text_color, undefined);
  assert.equal(mark.attrs.rounded, undefined);
  assert.equal(root.attrs.pl[0], 76);
});
test("search opens Projects and sidebar retains its original resizing commands", () => {
  const { panel, commands, stored } = fixture(), root = panel.render();
  nodes(root).find(n => n.attrs.id?.[0] === "search").handlers.click();
  assert.equal(commands[0].name, "trin-projects.picker");
  const icons = nodes(root).filter(n => n.kind === "svg").map(n => n.path);
  assert.ok(icons.includes("mods/workspace-chrome/search.svg"));
  assert.ok(icons.includes("mods/workspace-chrome/sidebar.svg"));
  nodes(root).find(n => n.attrs.id?.[0] === "sidebar-toggle").handlers.click();
  assert.equal(commands[1].name, "layout.resize");
  assert.equal(commands[1].args.px, 44);
  assert.equal(commands[2].args.id, "quotas");
  assert.equal(commands[2].args.px, 180);
  assert.equal(stored["sidebar.width"], 268);
});
test("macOS rail keeps traffic-light space clear", () => {
  const root = fixture(44).panel.render();
  assert.equal(nodes(root).some(n => n.kind === "image"), false);
  assert.equal(nodes(root).some(n => n.attrs.id?.[0] === "sidebar-toggle"), false);
});
