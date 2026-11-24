// Main sessions in Herdr's space/tab/pane order, with explicit pins first.

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { agents, terminals, spaces, run, theme, shared, setShared, setAgentOrder, setBadge, layout } from "herdr";
import { ActivityMonitor } from "./activity.js";
import { WorkingClock } from "./metrics.js";
import { spaceAvatar, statusIndicator, sessionDetails } from "./appearance.js";

function find(node, id) {
  if (node?.id === id) return node;
  for (const child of node?.children ?? []) {
    const found = find(child, id);
    if (found) return found;
  }
  return null;
}

// The sidebar's rail goes by its width, not a flag: dragging its edge narrows it too.
const narrow = () => (find(layout(), "sidebar")?.size?.px ?? Infinity) < 160;

// Match the minimum rail used by the header.
function toggleSidebar() {
  const width = find(layout(), "sidebar")?.size?.px;
  const collapsed = narrow();
  if (!collapsed) setShared("sidebar.width", width);
  run("layout.resize", { id: "sidebar", px: collapsed ? (shared("sidebar.width") ?? 268) : 44 });
}

// Match session tabs: an explicit tab name wins, otherwise use the live title.
function sessionName(tab, agent) {
  const title = (agent.title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
  return (tab?.named ? tab.name : title || agent.program?.name)
    || tab?.name || "Agent";
}
export default class Agents extends View {
  init(_props, cx) {
    this.pins = JSON.parse(localStorage.getItem("pins") ?? "[]");
    this.activity = new ActivityMonitor();
    this.clock = new WorkingClock();
    this.activity.refresh(cx);
    // Maintain topology/retry scheduling, not a 5s API poll. Each machine has
    // one outstanding SSE bridge call; healthy calls reconnect themselves.
    cx.timer.every(5000, cx => {
      this.activity.refresh(cx);
    });
    // A local data clock: working ALWAYS accrues time, including between
    // steps, during SSE reconnects, and while history is still loading.
    cx.timer.every(1000, cx => {
      if (!narrow() && this.working) cx.notify();
    });
  }

  close() {
    this.activity?.close();
    if (this.badge !== undefined) setBadge(null);
  }

  updateBadge(list) {
    // Seen results become idle; duplicate panes must not count a session twice.
    const count = new Set(list.filter(a => a.status === "blocked" || a.status === "done").map(a => a.address)).size;
    const text = count ? String(count) : null;
    if (this.badge === text) return;
    setBadge(text);
    this.badge = text;
  }

  setPins(pins) {
    this.pins = pins;
    localStorage.setItem("pins", JSON.stringify(pins));
  }

  // Drops pins of agents that ended. Only for machines the list has heard from:
  // right after a start, or while a machine connects, its agents aren't listed yet.
  prunePins(all) {
    const machine = (addr) => (addr.includes("/") ? addr.split("/")[0] : "");
    const heard = new Set(all.map((k) => machine(k.address)));
    const live = new Set(all.filter((k) => k.state === "live").map((k) => k.address));
    const kept = this.pins.filter((p) => !heard.has(machine(p)) || live.has(p));
    if (kept.length !== this.pins.length) this.setPins(kept);
  }

  render(cx) {
    const t = theme();
    // Herdr invalidates this view when terminals/titles change. Bootstrap the
    // new target immediately rather than waiting for the 5s retry timer.
    if (cx) this.activity?.refresh(cx);
    const collapsed = narrow();
    const all = terminals();
    // `agents()` can also contain a child that was kept as a tab. Its
    // parent link is independent of whether the terminal has its own pane.
    const children = new Set(all.filter(terminal => terminal.parent).map(terminal => terminal.address));
    const roots = agents().filter(a => !children.has(a.address));
    this.working = roots.some(a => a.status === "working");
    this.updateBadge(roots);
    const sp = spaces();
    const spaceById = new Map(sp.map(s => [s.id, s]));
    const projects = shared("trin.projects.v1")?.projects ?? [];
    const spaceOf = a => {
      const s = spaceById.get(a.space) ?? { id: a.space, name: a.space_name };
      const p = projects.find(p => p.space === a.space);
      return p ? { ...s, name: p.name, color: p.color } : s;
    };
    const focusAgent = a => {
      const state = shared("trin.projects.v1"), p = state?.projects?.find(p => p.space === a.space);
      if (p && state.selected !== p.id) setShared("trin.projects.v1", { ...state, selected: p.id });
      run("pane.focus", { pane: a.pane });
    };
    // Track work time even when the compact rail doesn't show it.
    this.clock.sync(roots, a => this.activity.details(a), Date.now());
    this.prunePins(all);
    const pinned = this.pins.map(p => roots.find(a => a.address === p)).filter(Boolean);
    const list = [...pinned, ...roots.filter(a => !this.pins.includes(a.address))];
    setAgentOrder(list.map((a) => a.address));

    const tabs = sp.flatMap(s => s.tabs);
    const label = (a) => sessionName(tabs.find(tab => tab.id === a.tab), a);

    const row = (a) => {
      const pic = spaceAvatar(spaceOf(a), t);
      if (collapsed) {
        const meta = this.activity?.details(a);
        const ready = a.status === "done", needsYou = a.status === "blocked";
        const attention = ready || needsYou;
        const signal = needsYou ? t.blocked : t.done;
        const description = [label(a), spaceOf(a).name || a.space_name,
          `${a.program?.name || "Agent"} · ${a.status}`,
          ready ? "Готово — откройте сессию, чтобы посмотреть результат" : needsYou ? "Нужно ваше действие — откройте сессию" : null,
          meta?.modelID ? `${meta.providerID ?? ""}/${meta.modelID}${meta.variant ? ` · ${meta.variant}` : ""}` : null,
          a.address, this.activity?.notice(a)].filter(Boolean).join("\n");
        return h_flex()
          .id(`ag-${a.address}`)
          .h(36)
          .flex_none()
          .items_center()
          .justify_center()
          .cursor_pointer()
          .tooltip(description)
          .on_click(() => focusAgent(a))
          .child(h_flex().relative().size(32).flex_none().rounded(7).items_center().justify_center()
            .when(a.focused && !attention, el => el.bg(t.selected).border(1).border_color(t.accent))
            .when(attention, el => el.bg(signal + "26").border(2).border_color(signal))
            .when(!a.focused && !attention, el => el.hover(s => s.bg(t.hover)))
            .child(pic.size(22).text_size(12).tooltip(description))
            .child(h_flex().absolute().right(-1).bottom(-1).size(16).rounded(5).bg(ready ? "#3b82f6" : attention ? signal : t.chrome)
              .items_center().justify_center()
              .when(ready, el => el.child(svg("mods/workspace-chrome/check.svg").size(12).text_color(t.chrome)))
              .when(needsYou, el => el.child(div().font_bold().text_size(12).text_color(t.chrome).child("!")))
              .when(!attention, el => el.child(statusIndicator(t, a.status)))));
      }
      const idle = a.status === "idle" && !a.focused;
      const isPinned = this.pins.includes(a.address);
      const pin = div()
        .id(`pin-${a.address}`)
        .flex_none()
        .p(2)
        .rounded(4)
        .text_color(t.dim)
        .cursor_pointer()
        .hover((s) => s.bg(t.hover).text_color(t.foreground))
        .tooltip(isPinned ? "Unpin" : "Pin to the top")
        .on_click((_e, cx) => {
          cx.stop_propagation();
          this.setPins(isPinned ? this.pins.filter((p) => p !== a.address) : [...this.pins, a.address]);
          // The row moves away from under the pointer, and no hover-out comes.
          this.hovered = null;
          cx.notify();
        })
        .child(svg(`mods/workspace-chrome/${isPinned ? "pin-off" : "pin"}.svg`).size(13));
      const name = label(a);
      const activity = this.activity?.get(a);
      const meta = this.activity?.details(a);
      const notice = this.activity?.notice(a);
      return v_flex()
        .id(`ag-${a.address}`)
        .px(8)
        .h(52)
        .py(6)
        .flex_none()
        .rounded(8)
        .gap(4)
        .when(a.focused, (el) => el.bg(t.selected))
        .cursor_pointer()
        .hover((s) => s.bg(t.hover))
        .on_hover((on, cx) => {
          if (on) this.hovered = a.address;
          else if (this.hovered === a.address) this.hovered = null;
          cx.notify();
        })
        .on_click(() => focusAgent(a))
        .child(h_flex().h(18).min_w(0).gap(7).items_center()
           .child(pic)
          .child(div().h(18).line_height("18px").flex_1().min_w(0).truncate().text_color(idle ? t.muted : t.foreground)
            .tooltip(notice ? `${name}\n${notice}` : name).child(name))
          .when(this.hovered === a.address, el => el.child(pin.bg(t.raised)))
          .child(statusIndicator(t, a.status)))
        .child(sessionDetails(t, meta, activity, this.clock.value(a.address), a.address));
    };

    return v_flex()
      .size_full()
      .min_h(0)
      .bg(t.chrome)
      .border_t(1)
      .border_color(t.border)
      .text_size(13)
      .child(v_flex().id("ag-list").flex_1().min_h(0).pt(6).px(collapsed ? 0 : 8).overflow_y_scrollbar()
        .when(!collapsed, el => el.mode("always"))
        .when(list.length === 0 && !collapsed, el =>
          el.child(div().px(4).text_size(12).text_color(t.muted).child("Нет сессий со статусом.")))
        .children(list.map(row)))
      .when(collapsed, (el) =>
        el.child(
          h_flex()
            .h(28)
            .items_center()
            .justify_center()
            .child(
              div()
                 .id("collapse")
                 .size(28).flex().items_center().justify_center().rounded(6)
                .text_size(13)
                .text_color(t.dim)
                .hover((s) => s.text_color(t.foreground))
                 .on_click(toggleSidebar)
                 .tooltip("Развернуть боковую панель")
                 .child(svg("mods/workspace-chrome/panel-left.svg").size(15)),
            ),
        ),
      );
  }
}
// Modified derivative of Herdr agents (Apache-2.0): custom metrics and UI.
// Original modifications: MIT. See repository THIRD_PARTY.md.
