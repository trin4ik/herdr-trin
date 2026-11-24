// Main sessions with a manual sidebar order independent of terminal tabs.

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { agents, terminals, spaces, run, theme, shared, setShared, setAgentOrder, setBadge, layout, checking } from "herdr";
import { ActivityMonitor, sessionAddress, terminalSessionID } from "./activity.js";
import { WorkingClock } from "./metrics.js";
import { spaceAvatar, statusIndicator, sessionDetails } from "./appearance.js";
import { parseOrder, orderedSessions, moveSession } from "./order.js";

const ORDER_KEY = "sessions.order";
const TITLES_KEY = "trin.sessions.titles";
const DRAG_THRESHOLD = 4;
const COMPLETION_BLUE = "#3b82f6";
const ATTENTION = { blocked: 3, done: 2, working: 1 };

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
function sessionName(tab, agent, canonical) {
  const title = (agent.title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
  return (tab?.named ? tab.name : canonical || title || agent.program?.name)
    || tab?.name || "Agent";
}
export default class Agents extends View {
  init(_props, cx) {
    this.order = parseOrder(localStorage.getItem(ORDER_KEY) ?? localStorage.getItem("pins"));
    this.drag = null;
    this.activity = new ActivityMonitor();
    this.clock = new WorkingClock();
    this.live = !checking();
    if (!this.live) return;
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
    this.drag = null;
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

  cancelDrag(cx) {
    const redraw = this.drag?.active;
    this.drag = null;
    if (redraw) cx.notify();
  }

  dragTarget(event, id, cx) {
    const drag = this.drag;
    if (!drag) return;
    const { x, y } = event.position;
    const active = drag.active || Math.hypot(x - drag.start.x, y - drag.start.y) >= DRAG_THRESHOLD;
    const over = active && id !== drag.id ? id : null;
    const after = Boolean(over && event.local_position && event.bounds && event.local_position.y >= event.bounds.height / 2);
    if (active !== drag.active || over !== drag.over || after !== drag.after) {
      Object.assign(drag, { active, over, after });
      cx.notify();
    }
  }

  sortable(row, { id, agent }, title, collapsed, t, select) {
    const sorting = this.rows.length > 1;
    return row.relative().accessibility_label(title)
      .when(sorting, el => el.cursor_grab())
      .when(!sorting && !agent.background, el => el.cursor_pointer())
      .when(this.drag?.active && this.drag.id === id, el => el.opacity(0.5).cursor_grabbing())
      .when(this.drag?.over === id, el => el.child(div().id(`session-drop-${agent.address}`).absolute()
        .left(collapsed ? 4 : 8).right(collapsed ? 4 : 8).h(2).rounded(1).bg(t.accent)
        .when(this.drag.after, line => line.bottom(0)).when(!this.drag.after, line => line.top(0))))
      .on_mouse_down("left", (event, cx) => {
        cx.stop_propagation();
        this.cancelDrag(cx);
        this.drag = { id, start: event.position, active: false, over: null, after: false };
      })
      .on_mouse_move((event, cx) => {
        if (!this.drag) return;
        cx.stop_propagation();
        this.dragTarget(event, id, cx);
      })
      .on_mouse_up("left", (event, cx) => {
        if (!this.drag) return;
        cx.stop_propagation();
        this.dragTarget(event, id, cx);
        const { id: source, active, over, after } = this.drag;
        this.cancelDrag(cx);
        if (!active && source === id && !agent.background) select(agent);
        else if (active && over && event.local_position && event.bounds) {
          const order = moveSession(this.rows, this.renderOrder, source, over, after);
          if (order !== this.renderOrder) {
            this.order = order;
            localStorage.setItem(ORDER_KEY, JSON.stringify(order));
          }
        }
      });
  }

  render(cx) {
    const t = theme();
    // Herdr invalidates this view when terminals/titles change. Bootstrap the
    // new target immediately rather than waiting for the 5s retry timer.
    if (cx && this.live) this.activity?.refresh(cx);
    const collapsed = narrow();
    const all = new Map(terminals().map(terminal => [terminal.address, terminal]));
    const sp = spaces();
    const spaceById = new Map(sp.map(s => [s.id, s]));
    const aliases = new Map(), attached = new Map(), sessionIDs = new Map(), excluded = new Set();
    const titles = {};
    // `agents()` can also contain a child that was kept as a tab. Its
    // parent link is independent of whether the terminal has its own pane.
    for (const agent of agents()) {
      const terminal = all.get(agent.address);
      if (terminal?.parent) continue;
      const sessionID = terminalSessionID(terminal);
      const machine = spaceById.get(agent.space)?.machine ?? null;
      const id = sessionID ? sessionAddress(machine, sessionID) : agent.address;
      const session = this.activity.session?.(agent, sessionID);
      if (session?.excluded) {
        excluded.add(id);
        continue;
      }
      if (sessionID) {
        sessionIDs.set(agent.address, sessionID);
        aliases.set(agent.address, id);
        const title = typeof session?.sessionTitle === "string"
          ? [...session.sessionTitle.replace(/[\x00-\x1f\x7f-\x9f]/g, " ").trim()].slice(0, 256).join("") : "";
        if (title) titles[agent.address] = { sessionID, title };
      }
      const previous = attached.get(id)?.agent;
      if (!previous || (agent.focused && !previous.focused) || (agent.focused === previous.focused
        && (ATTENTION[agent.status] ?? 0) > (ATTENTION[previous.status] ?? 0))) {
        attached.set(id, { id, agent });
      }
    }
    const titleSignature = JSON.stringify(titles);
    if (this.live && titleSignature !== this.titleSignature) {
      this.titleSignature = titleSignature;
      setShared(TITLES_KEY, titles);
    }
    const roots = [...attached.values()].map(row => row.agent);
    this.working = roots.some(a => a.status === "working");
    if (this.live !== false) this.updateBadge(roots);
    const projects = shared("trin.projects.v1")?.projects ?? [];
    const spaceOf = a => {
      if (a.background) {
        const at = projects.filter(p => p.folder === a.folder && p.machine === a.machine);
        return at.length === 1 ? at[0] : { name: a.folder?.split(/[\\/]/).filter(Boolean).at(-1) || "OpenCode" };
      }
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
    this.clock.sync(roots, a => this.activity.details(a, sessionIDs.get(a.address)), Date.now());
    for (const agent of this.activity.backgroundRows?.() ?? []) {
      if (!attached.has(agent.address) && !excluded.has(agent.address)) {
        attached.set(agent.address, { id: agent.address, agent });
      }
    }
    const rows = [...attached.values()];
    this.renderOrder = [...new Set(this.order.map(id => aliases.get(id) ?? id))];
    this.rows = orderedSessions(rows, this.renderOrder);
    if (this.drag && (!rows.some(row => row.id === this.drag.id)
      || (this.drag.over && !rows.some(row => row.id === this.drag.over)))) this.drag = null;
    if (this.live !== false) setAgentOrder(this.rows.filter(row => !row.agent.background).map(row => row.agent.address));

    const tabs = sp.flatMap(s => s.tabs);
    const label = (a) => sessionName(tabs.find(tab => tab.id === a.tab), a, titles[a.address]?.title);

    const row = (a) => {
      const pic = spaceAvatar(spaceOf(a), t);
      const sid = sessionIDs.get(a.address);
      const meta = a.background ? a.meta : this.activity?.details(a, sid);
      const backgroundHint = a.background ? `Фоновая сессия · ${a.machine ?? "Этот компьютер"}` : null;
      const ready = a.status === "done";
      if (collapsed) {
        const needsYou = a.status === "blocked";
        const attention = ready || needsYou;
        const signal = needsYou ? t.blocked : t.done;
        const description = [label(a), spaceOf(a).name || a.space_name,
          `${a.program?.name || "Agent"} · ${a.status}`,
          ready ? "Готово — откройте сессию, чтобы посмотреть результат" : needsYou ? "Нужно ваше действие — откройте сессию" : null,
          meta?.modelID ? `${meta.providerID ?? ""}/${meta.modelID}${meta.variant ? ` · ${meta.variant}` : ""}` : null,
          backgroundHint ?? a.address, this.activity?.notice(a, sid)].filter(Boolean).join("\n");
        return h_flex()
          .id(`ag-${a.address}`)
          .h(36)
          .flex_none()
          .items_center()
          .justify_center()
          .tooltip(description)
          .child(h_flex().relative().size(32).flex_none().rounded(7).items_center().justify_center()
            .when(a.focused && !attention, el => el.bg(t.selected).border(1).border_color(t.accent))
            .when(attention, el => el.bg(signal + "26").border(2).border_color(signal))
            .when(!a.focused && !attention, el => el.hover(s => s.bg(t.hover)))
            .child(pic.size(22).text_size(12).tooltip(description))
            .child(h_flex().absolute().right(-1).bottom(-1).size(16).rounded(5).bg(ready ? COMPLETION_BLUE : attention ? signal : t.chrome)
              .items_center().justify_center()
              .when(ready, el => el.child(svg("mods/workspace-chrome/check.svg").size(12).text_color(t.chrome)))
              .when(needsYou, el => el.child(div().font_bold().text_size(12).text_color(t.chrome).child("!")))
              .when(!attention, el => el.child(statusIndicator(t, a.status)))));
      }
      const idle = a.status === "idle" && !a.focused;
      const name = label(a);
      const activity = this.activity?.get(a, sid);
      const notice = backgroundHint ?? this.activity?.notice(a, sid);
      return v_flex()
        .id(`ag-${a.address}`)
        .px(8)
        .h(52)
        .py(6)
        .flex_none()
        .rounded(8)
        .gap(4)
        .when(a.focused && !ready, el => el.bg(t.selected))
        .when(ready, el => el.bg(COMPLETION_BLUE + "30"))
        .hover(s => s.bg(ready ? COMPLETION_BLUE + "3b" : t.hover))
        .child(h_flex().h(18).min_w(0).gap(7).items_center()
           .child(pic)
          .child(div().h(18).line_height("18px").flex_1().min_w(0).truncate().text_color(idle ? t.muted : t.foreground)
            .tooltip(notice ? `${name}\n${notice}` : name).child(name))
          .child(statusIndicator(t, a.status)))
        .child(sessionDetails(t, meta, activity, this.clock.value(a.address), a.address, a.background));
    };

    return v_flex()
      .size_full()
      .min_h(0)
      .bg(t.chrome)
      .border_t(1)
      .border_color(t.border)
      .text_size(13)
      .when(!collapsed, el => el.child(h_flex().id("sessions-title").h(28).flex_none().pl(16).pr(10).items_center()
        .text_size(11).text_color(t.dim)
        .child(div().flex_1().min_w(0).truncate().child("Сессии")
          .when(this.rows.length > 1, title => title.tooltip("Перетащите строку выше или ниже, чтобы изменить порядок")))))
      .child(v_flex().id("ag-list").flex_1().min_h(0).pt(collapsed ? 6 : 0).px(collapsed ? 0 : 8).overflow_y_scrollbar()
        .when(!collapsed, el => el.mode("always"))
        .on_mouse_move((event, cx) => this.dragTarget(event, null, cx))
        .on_mouse_up("left", (_event, cx) => this.cancelDrag(cx))
        .on_mouse_down_out((_event, cx) => this.cancelDrag(cx))
        .on_hover((over, cx) => { if (!over) this.cancelDrag(cx); })
        .when(this.rows.length === 0 && !collapsed, el =>
          el.child(div().px(4).text_size(12).text_color(t.muted).child("Нет сессий со статусом.")))
        .children(this.rows.map(item => this.sortable(row(item.agent), item, label(item.agent), collapsed, t, focusAgent))))
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
