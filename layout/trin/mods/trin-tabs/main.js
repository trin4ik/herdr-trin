import { View, div, svg } from "gpui-kit";
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
    this.drag = null;
    this.widths = new Map();
    this.panels = JSON.parse(localStorage.getItem("panels") ?? "{}");
  }

  close() { this.drag = null; }

  cancelDrag(cx) {
    if (!this.drag) return;
    this.drag = null;
    cx.notify();
  }

  beginDrag(tab, space, event, cx) {
    cx.stop_propagation();
    if (event.click_count === 2) { this.cancelDrag(cx); run("tab.rename", { id: tab.id }); return; }
    if (!event.bounds || !event.position) return;
    const ids = space.tabs.map(t => t.id);
    const widths = ids.map(id => id === tab.id ? event.bounds.width : this.widths.get(id) ?? 110);
    const from = ids.indexOf(tab.id);
    const left = widths.slice(0, from).reduce((sum, w) => sum + w, 0);
    this.drag = { id: tab.id, space: space.id, ids, widths, from, to: from,
      left, origin: event.position.x, x: 0, active: false };
    // Freeze geometry on the press, before a loading agent can rename its tab.
    cx.notify();
  }

  moveDrag(event, cx) {
    const d = this.drag;
    if (!d) return;
    const current = spaces().find(s => s.id === d.space)?.tabs ?? [];
    if (current.map(t => t.id).join("|") !== d.ids.join("|")) { this.cancelDrag(cx); return; }
    const dx = event.position.x - d.origin;
    if (!d.active && Math.abs(dx) < 5) return;
    d.active = true;
    const total = d.widths.reduce((sum, w) => sum + w, 0), width = d.widths[d.from];
    d.x = Math.max(-d.left, Math.min(dx, total - d.left - width));
    const center = d.left + dx + width / 2;
    let x = 0, to = 0;
    d.widths.forEach((w, i) => { if (i !== d.from && center > x + w / 2) to++; x += w; });
    d.to = to;
    cx.notify();
  }

  finishDrag(cx) {
    const d = this.drag;
    if (!d) return;
    this.drag = null;
    cx.stop_propagation();
    const current = spaces().find(s => s.id === d.space)?.tabs ?? [];
    if (current.map(t => t.id).join("|") === d.ids.join("|")) {
      if (!d.active) run("tab.select", { id: d.id });
      else if (d.to !== d.from) run("tab.move", { id: d.id, index: d.to + 1 });
    }
    cx.notify();
  }

  offset(id) {
    const d = this.drag;
    if (!d?.active) return 0;
    const i = d.ids.indexOf(id), width = d.widths[d.from];
    if (d.to > d.from && i > d.from && i <= d.to) return -width;
    if (d.to < d.from && i >= d.to && i < d.from) return width;
    return 0;
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
    this.widths ??= new Map();
    if (this.drag && (space?.id !== this.drag.space || shown.map(t => t.id).join("|") !== this.drag.ids.join("|"))) this.drag = null;
    for (const id of this.widths.keys()) if (!shown.some(t => t.id === id)) this.widths.delete(id);
    const stop = (_e, cx) => cx.stop_propagation();
    const button = (id, label, action) => div().id(id).size(26).flex_none().flex().items_center().justify_center().rounded(6).text_color(t.dim).hover(s => s.bg(t.hover).text_color(t.foreground))
      .on_mouse_down("left", stop).on_click(action).child(label);
    const item = (c, ghost = false) => {
      const tabPanes = panes().filter(p => p.tab === c.id);
      const pane = tabPanes.find(p => p.program?.agent) ?? tabPanes.find(p => p.focused) ?? tabPanes[0];
      const program = pane?.program;
      const title = (pane?.title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
      const name = c.named ? c.name : title || program?.name || "Terminal";
      return h_flex().id(ghost ? "tab-drag-ghost" : `tab-${c.id}`).relative().h_full().min_w(110).px(14).gap(8).items_center().flex_none().text_size(13)
        .when(!ghost, el => el.left(this.offset(c.id)).transition("left", { duration: this.drag?.active ? 140 : 0, easing: "ease-out" })
          .when(Boolean(this.drag), el => el.w(this.drag.widths[this.drag.ids.indexOf(c.id)]))
          .opacity(this.drag?.active && this.drag.id === c.id ? 0 : 1)
          .on_size_change(size => {
            // During a drag the widths belong to its immutable geometry, not
            // a title/agent-status update or an in-flight layout callback.
            if (!this.drag) this.widths.set(c.id, size.width);
          }))
        .border_r(1).border_b(c.active ? 0 : 1).border_color(t.border)
        .bg(c.active ? t.background : t.chrome).text_color(c.active ? t.foreground : t.muted)
        .when(c.active, el => el.child(div().absolute().top(0).left(0).right(0).h(2).bg(t.accent)))
        .hover(s => s.bg(c.active ? t.background : t.hover))
        .when(!ghost, el => el.on_mouse_down("left", (e, cx) => this.beginDrag(c, space, e, cx))
          .on_mouse_down("right", (_e, cx) => { cx.stop_propagation(); this.cancelDrag(cx); openOverlay({ mod: "trin-tab-menu", kind: "popup", width: 220, props: { target: { id: c.id, space: space.id, project: project?.id } } }); }))
        .child(div().min_w(0).flex_shrink(1).max_w(170).truncate().tooltip(name).child(name))
        .child(div().id(ghost ? "tab-drag-close" : `close-${c.id}`).flex_none().text_color(t.dim)
          .on_mouse_down("left", stop).on_click((_e, cx) => { cx.stop_propagation(); this.cancelDrag(cx); run("tab.close", { id: c.id }); }).child(svg("mods/pane/close.svg").size(12)));
    };
    const root = layout(), sidebar = find(root, "sidebar"), w = window();
    const newTabShortcut = w?.platform === "macos" ? "⌘ T" : "Ctrl T";
    const terminalShortcut = w?.platform === "macos" ? "⇧ ⌘ T" : "Ctrl Shift T";
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
    return h_flex().id("tabs-root").size_full().min_w(0).pr(10).pl(Math.max(0, lightsPadding - 10)).gap(8).items_center().bg(t.chrome)
      .on_size_change((size, cx) => {
        const old = this.viewportWidth; this.viewportWidth = size.width;
        if (old != null && Math.abs(old - size.width) > 0.5) this.cancelDrag(cx);
      })
      .on_mouse_move((e, cx) => this.moveDrag(e, cx))
      .on_mouse_up("left", (_e, cx) => this.finishDrag(cx))
      .on_mouse_down_out((_e, cx) => this.cancelDrag(cx))
      .on_hover((over, cx) => { if (!over) this.cancelDrag(cx); })
      .on_scroll_wheel(e => { if (!this.drag && space && Math.abs(e.delta.y) >= 1) run(e.delta.y > 0 ? "trin-projects.tab-prev" : "trin-projects.tab-next"); })
      // Natural-width tabs keep New beside the last tab; only the tab list
      // shrinks/scrolls when full, leaving New and panel controls reachable.
      .child(h_flex().id("tab-strip").flex_1().min_w(0).h_full().gap(8).items_center()
        .child(h_flex().id("tab-scroll").relative().min_w(0).h_full().items_center().gap(0).overflow_x_scrollbar().w("auto").flex_none().flex_shrink(1)
          .children(shown.map(c => item(c)))
          .when(this.drag?.active, el => el.child(item(shown.find(c => c.id === this.drag.id), true)
            .absolute().top(0).left(this.drag.left + this.drag.x).w(this.drag.widths[this.drag.from]).shadow_md().bg(t.raised))))
        .child(h_flex().id("tab-create").flex_none().gap(14).items_center()
          .child(h_flex().id("tab-new").h(26).px(4).gap(6).flex_none().items_center().rounded(4)
            .text_color(t.muted).hover(s => s.bg(t.hover).text_color(t.foreground))
            .tooltip(`Новая сессия · opencode --auto · ${newTabShortcut}`)
            .on_mouse_down("left", stop).on_click(() => run("trin-projects.new-tab"))
            .child(svg("assets/trin-providers/opencode-logo.svg").w(8).h(10).text_color(t.muted).opacity(0.75))
            .child(div().id("tab-new-shortcut").flex_none().text_size(11).text_color(t.dim).opacity(0.65).child(newTabShortcut)))
          .child(h_flex().id("tab-terminal").h(26).px(4).gap(6).flex_none().items_center().rounded(4)
            .text_color(t.muted).hover(s => s.bg(t.hover).text_color(t.foreground))
            .tooltip(`Новый терминал · ${terminalShortcut}`)
            .on_mouse_down("left", stop).on_click(() => run("trin-projects.new-terminal"))
            .child(svg("mods/workspace-chrome/terminal.svg").size(12).text_color(t.muted).opacity(0.75))
            .child(div().id("tab-terminal-shortcut").flex_none().text_size(11).text_color(t.dim).opacity(0.65).child(terminalShortcut)))))
      .when(Boolean(space) && enabled.includes("files"), el => el.child(panelButton("files", "Files", "mods/files/file.svg")))
      .when(Boolean(space) && enabled.includes("changes"), el => el.child(panelButton("changes", "Changes", "mods/changes/changes.svg")))
      .when(w?.buttons, el => el.child(button("minimize", w.platform === "windows" ? svg("mods/trin-tabs/win-min.svg").size(16) : "−", () => run("window.minimize")))
        .child(button("maximize", w.platform === "windows" ? svg(`mods/trin-tabs/${w.maximized ? "win-restore" : "win-max"}.svg`).size(16) : "□", () => run("window.maximize")))
        .child(button("close-window", w.platform === "windows" ? svg("mods/trin-tabs/win-close.svg").size(16) : svg("mods/pane/close.svg").size(13), () => run("window.close"))));
  }
}
// Modified derivative of Herdr tabs (Apache-2.0): custom tabs and project navigation.
// Original modifications: MIT. See repository THIRD_PARTY.md.
