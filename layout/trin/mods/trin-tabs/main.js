import { View, div, svg, image } from "gpui-kit";
import { h_flex } from "gpui-base";
import { spaces, panes, run, openOverlay, theme, window, shared, layout, settings } from "herdr";

function find(node, id) {
  if (node?.id === id) return node;
  for (const child of node?.children ?? []) {
    const found = find(child, id);
    if (found) return found;
  }
  return null;
}

export default class Tabs extends View {
  init() {
    this.pressed = null;
    this.panels = JSON.parse(localStorage.getItem("panels") ?? "{}");
  }

  // Preserve the original Files/Changes choice per space.
  syncPanel(space, panel, body, enabled) {
    if (!space || !panel) return;
    const now = panel.hidden ? null : body?.active ?? null;
    if (space.id !== this.space) {
      const first = this.space === undefined;
      this.space = space.id; this.pending = null;
      if (!(first && !(space.id in this.panels))) {
        const stored = this.panels[space.id] ?? null, want = enabled.includes(stored) ? stored : null;
        if (want !== now) {
          this.pending = { want, until: Date.now() + 1000 };
          if (want) { run("layout.select", { stack: "panel-body", child: want }); run("layout.show", { id: "panel" }); }
          else run("layout.hide", { id: "panel" });
          return;
        }
      }
    }
    if (this.pending) {
      if (this.pending.want !== now && Date.now() < this.pending.until) return;
      this.pending = null;
    }
    if (this.panels[space.id] === now) return;
    this.panels[space.id] = now;
    localStorage.setItem("panels", JSON.stringify(this.panels));
  }

  render() {
    const t = theme();
    const projects = shared("trin.projects.v1");
    const project = projects?.projects?.find(p => p.id === projects.selected);
    const space = projects ? spaces().find(s => s.id === project?.space) : spaces().find(p => p.active);
    const shown = space?.tabs ?? [];
    const stop = (_e, cx) => cx.stop_propagation();
    const button = (id, label, action) => div().id(id).size(26).flex_none().flex().items_center().justify_center().rounded(6).text_color(t.dim).hover(s => s.bg(t.hover).text_color(t.foreground))
      .on_mouse_down("left", stop).on_click(action).child(label);
    const item = c => {
      const tabPanes = panes().filter(p => p.tab === c.id);
      const pane = tabPanes.find(p => p.program?.agent) ?? tabPanes.find(p => p.focused) ?? tabPanes[0];
      const program = pane?.program;
      const title = (pane?.title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
      const name = c.named ? c.name : title || program?.name || "Terminal";
      return h_flex().id(`tab-${c.id}`).relative().h_full().min_w(110).px(14).gap(8).items_center().flex_none().text_size(13)
        .border_r(1).border_b(c.active ? 0 : 1).border_color(t.border)
        .bg(c.active ? t.background : t.chrome).text_color(c.active ? t.foreground : t.muted)
        .when(c.active, el => el.child(div().absolute().top(0).left(0).right(0).h(2).bg(t.accent)))
        .hover(s => s.bg(c.active ? t.background : t.hover))
        .on_mouse_down("left", (e, cx) => { cx.stop_propagation(); this.pressed = c.id; if (e.click_count === 2) run("tab.rename", { id: c.id }); })
        .on_mouse_up("left", () => {
          const from = this.pressed; this.pressed = null;
          const current = spaces().find(s => s.id === space.id)?.tabs ?? [];
          const target = current.findIndex(tab => tab.id === c.id);
          if (!current.some(tab => tab.id === from) || target < 0) return;
          if (from !== c.id) run("tab.move", { id: from, index: target + 1 });
          else run("tab.select", { id: c.id });
        })
        .on_mouse_down("right", (_e, cx) => { cx.stop_propagation(); openOverlay({ mod: "trin-tab-menu", kind: "popup", width: 220, props: { target: { id: c.id, space: space.id, project: project?.id } } }); })
        .when(Boolean(program?.icon), el => el.child(program.tint ? svg(program.icon).size(14).text_color(t.muted) : image(program.icon).size(14)))
        .child(div().max_w(170).truncate().child(c.zoomed ? `${name} Z` : name))
        .when(c.status === "working", el => el.child(svg("mods/workspace-chrome/spinner.svg").size(12).flex_none().text_color(t.working).spin(900)))
        .when(c.status === "blocked" || c.status === "done", el => el.child(div().size(6).rounded(3).bg(t[c.status])))
        .when(c.active, el => el.child(div().id(`close-${c.id}`).text_color(t.dim).on_mouse_down("left", stop).on_click((_e, cx) => { cx.stop_propagation(); run("tab.close", { id: c.id }); }).child(svg("mods/pane/close.svg").size(12))));
    };
    const root = layout(), sidebar = find(root, "sidebar"), w = window();
    const newTabShortcut = w?.platform === "macos" ? "⌘ T" : "Ctrl T";
    // Old saved rails and a dragged-narrow sidebar can be smaller than the window buttons.
    const lightsPadding = w?.platform === "macos" && w.titlebar === "none" && !w.fullscreen && root?.traffic_lights
      ? Math.max(10, (root.traffic_lights[0] + 64) / settings().zoom - (sidebar?.hidden ? 0 : sidebar?.size?.px ?? 0))
      : 10;
    const panel = find(root, "panel"), body = find(root, "panel-body");
    const enabled = shared("workspace.panels") ?? ["files", "changes"];
    // Panel preferences belong to the stable project, not a disposable Space ID.
    this.syncPanel(project ? { id: project.id } : space, panel, body, enabled);
    const panelButton = (id, label, icon) => {
      const on = panel && !panel.hidden && body?.active === id;
      return h_flex().id(`panel-${id}`).h(26).px(8).gap(5).flex_none().items_center().rounded(6)
        .text_size(12).bg(on ? t.selected : t.chrome).text_color(on ? t.foreground : t.muted)
        .hover(s => s.bg(t.hover)).on_mouse_down("left", stop)
        .on_click(() => {
          if (on) run("layout.hide", { id: "panel" });
          else { run("layout.select", { stack: "panel-body", child: id }); run("layout.show", { id: "panel" }); }
        }).child(svg(icon).size(14)).child(label);
    };
    return h_flex().size_full().min_w(0).pr(10).pl(Math.max(0, lightsPadding - 10)).gap(8).items_center().bg(t.chrome)
      .on_mouse_up("left", () => { this.pressed = null; })
      .on_scroll_wheel(e => { if (space && Math.abs(e.delta.y) >= 1) run(e.delta.y > 0 ? "trin-projects.tab-prev" : "trin-projects.tab-next"); })
      // Natural-width tabs keep New beside the last tab; only the tab list
      // shrinks/scrolls when full, leaving New and panel controls reachable.
      .child(h_flex().id("tab-strip").flex_1().min_w(0).h_full().gap(8).items_center()
        .child(h_flex().id("tab-scroll").min_w(0).h_full().items_center().gap(0).overflow_x_scrollbar().w("auto").flex_none().flex_shrink(1).children(shown.map(item)))
        .child(h_flex().id("tab-create").flex_none().gap(6).items_center()
          .child(button("tab-new", svg("mods/workspace-chrome/plus.svg").size(14), () => run("trin-projects.new-tab"))
            .size(18).rounded(4).border(1).border_color(t.border).bg(t.raised).text_color(t.muted).tooltip(`Новая вкладка · ${newTabShortcut}`))
          .child(div().id("tab-new-shortcut").flex_none().text_size(11).text_color(t.dim).opacity(0.65).child(newTabShortcut))))
      .when(Boolean(space) && enabled.includes("files"), el => el.child(panelButton("files", "Files", "mods/files/file.svg")))
      .when(Boolean(space) && enabled.includes("changes"), el => el.child(panelButton("changes", "Changes", "mods/changes/changes.svg")))
      .when(w?.buttons, el => el.child(button("minimize", w.platform === "windows" ? svg("mods/trin-tabs/win-min.svg").size(16) : "−", () => run("window.minimize")))
        .child(button("maximize", w.platform === "windows" ? svg(`mods/trin-tabs/${w.maximized ? "win-restore" : "win-max"}.svg`).size(16) : "□", () => run("window.maximize")))
        .child(button("close-window", w.platform === "windows" ? svg("mods/trin-tabs/win-close.svg").size(16) : svg("mods/pane/close.svg").size(13), () => run("window.close"))));
  }
}
// Modified derivative of Herdr tabs (Apache-2.0): custom tabs and project navigation.
// Original modifications: MIT. See repository THIRD_PARTY.md.
