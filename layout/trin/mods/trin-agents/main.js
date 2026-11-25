// Main sessions with a manual sidebar order independent of terminal tabs.

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { agents, terminals, panes, spaces, machines, exec, notify, run, theme, shared, setShared, setAgentOrder, setBadge, layout, checking } from "herdr";
import { ActivityMonitor } from "./activity.js";
import { attachedAgents, isOpenCode, processArgsKey, sessionAddress } from "./identity.js";
import { BackgroundAdmission } from "./background.js";
import { BackgroundOpens } from "./opening.js";
import { WorkingClock } from "./metrics.js";
import { modelDescription, spaceAvatar, statusIndicator, sessionDetails } from "./appearance.js";
import { parseOrder, orderedSessions, moveSession } from "./order.js";

const ORDER_KEY = "sessions.order";
const ATTACHED_KEY = "sessions.attached";
const TITLES_KEY = "trin.sessions.titles";
const DRAG_THRESHOLD = 4;
const COMPLETION_BLUE = "#3b82f6";
const ATTENTION = { blocked: 3, done: 2, working: 1 };
const WIDE_ROW_HEIGHT = 52;
const RAIL_ROW_HEIGHT = 36;
const HIDDEN_HEADER_HEIGHT = 30;
const HIDDEN_VISIBLE_ROWS = 3;
const HIDDEN_ROW_BOTTOM_PADDING = 5;

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

// Match session tabs: the bound OpenCode session owns its displayed name.
function sessionName(tab, agent, canonical) {
  const title = (agent.title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
  return canonical || (tab?.named ? tab.name : title || agent.program?.name)
    || tab?.name || "Agent";
}
// A native caption alone says nothing about the program behind a settled
// tab. Only synthesize a row for a complete set of ordinary panes.
function plainTabKey(tab, ps, ts) {
  const list = ps.filter(p => p.tab === tab.id);
  if (!list.length || list.length !== tab.panes || !list.every(p => p.mod ||
    (p.program?.agent === false && ts.has(p.terminal) && !isOpenCode(p, ts.get(p.terminal))))) return null;
  return JSON.stringify(list.map(p => [p.pane, p.terminal, p.mod, ts.get(p.terminal)?.created_ms]));
}
export default class Agents extends View {
  init(_props, cx) {
    this.order = parseOrder(localStorage.getItem(ORDER_KEY) ?? localStorage.getItem("pins"));
    this.drag = null;
    this.hiddenOpen = localStorage.getItem("sessions.hidden-open") === "true";
    this.opening = new BackgroundOpens();
    this.backgroundAdmission = new BackgroundAdmission(parseOrder(localStorage.getItem(ATTACHED_KEY)));
    this.activity = new ActivityMonitor(shared(TITLES_KEY) ?? {});
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
    this.closed = true;
    this.backgroundTimer?.cancel();
    this.activity?.close();
    if (this.badge !== undefined) setBadge(null);
  }

  currentAttachment(agent, expected) {
    const sp = spaces(), all = new Map(terminals().map(t => [t.address, t]));
    const space = sp.find(s => s.id === agent.space && s.tabs.some(t => t.id === agent.tab));
    const ps = panes();
    if (!all.has(agent.address) || !ps.some(p => p.pane === agent.pane && p.tab === agent.tab && p.terminal === agent.address)) return false;
    const current = attachedAgents(agents(), ps, all, sp).find(a => a.address === agent.address
      && a.pane === agent.pane && a.space === agent.space && a.tab === agent.tab);
    if (!current || !space) return false;
    return (this.activity.boundSessionID(current, all.get(current.address), false, space.machine ?? null) ?? null) === (expected ?? null);
  }

  openBackground(agent, cx) {
    this.opening ??= new BackgroundOpens();
    // Attached status and the background list arrive independently. A click
    // from the old frame must not launch again before the next render.
    const sp = spaces(), ts = new Map(terminals().map(t => [t.address, t]));
    const registry = shared("trin.projects.v1");
    const homes = (registry?.projects ?? []).filter(p => (p.machine ?? null) === (agent.machine ?? null) && p.folder === agent.folder);
    const owners = attachedAgents(agents(), panes(), ts, sp).filter(a => {
      const s = sp.find(s => s.id === a.space && s.tabs.some(t => t.id === a.tab));
      return s && (s.machine ?? null) === (agent.machine ?? null) && !ts.get(a.address)?.parent
        && this.activity.boundSessionID(a, ts.get(a.address), false, s.machine) === agent.sessionID;
    });
    const last = this.opening.attached.get(agent.address);
    const candidates = homes.length === 1 && owners.some(a => a.space === homes[0].space)
      ? owners.filter(a => a.space === homes[0].space) : owners;
    if (!candidates.length && last && ts.has(last.address)
      && this.activity.boundSessionID(last, ts.get(last.address), false, agent.machine) === agent.sessionID) return;
    const existing = candidates.find(a => a.address === last?.address) ?? candidates[0] ?? this.opening.pending.get(agent.address);
    if (existing) {
      if (existing.pane && !this.currentAttachment(existing, agent.sessionID)) return;
      const project = registry?.projects?.find(p => p.space === existing.space);
      if (project && project.id !== registry.selected) setShared("trin.projects.v1", { ...registry, selected: project.id });
      if (existing.pane) run("pane.focus", { pane: existing.pane });
      else if (existing.tab && sp.some(s => s.tabs.some(t => t.id === existing.tab))) run("tab.select", { id: existing.tab });
      return;
    }
    const { sessionID, folder, machine } = agent;
    if (!/^ses[A-Za-z0-9_-]+$/.test(sessionID ?? "") || typeof folder !== "string"
      || !/^(?:\/|[A-Za-z]:[\\/]|\\\\)/.test(folder) || /[\x00-\x1f]/.test(folder)) {
      notify({ title: "Сессии", body: "Не удалось определить каталог сессии.", level: "error" });
      return;
    }
    if (!machines().some(m => (m.name ?? null) === (machine ?? null) && m.layout === "ready")) {
      notify({ title: "Сессии", body: "Машина с этой сессией сейчас недоступна.", level: "error" });
      return;
    }
    const scoped = sp.filter(s => (s.machine ?? null) === (machine ?? null));
    const folderOf = s => registry?.projects?.find(p => p.space === s.id)?.folder ?? s.folder;
    const matches = scoped.filter(s => folderOf(s) === folder);
    const space = matches.length === 1 ? matches[0] : null;
    const target = this.opening.begin(agent.address);
    cx.spawn(async cx => {
      try {
        const argv = ["opencode", "--session", sessionID];
        const args = space ? { space: space.id, folder, argv, focus: false }
          : { machine: machine ?? "", folder, argv, focus: false };
        const result = await exec("herdr-app", ["do", space ? "tab.new" : "space.new", JSON.stringify(args)]);
        if (result.code !== 0) throw Error("Не удалось открыть сессию OpenCode.");
        const reply = JSON.parse(result.stdout);
        if (!reply.tab || reply.error) throw Error("Herdr не подтвердил открытие вкладки.");
        this.opening.acknowledge(target, reply, Date.now());
        if (this.closed || shared("trin.projects.v1")?.selected !== registry?.selected) return;
        const state = shared("trin.projects.v1"), project = state?.projects?.find(p => p.space === (reply.space ?? space?.id));
        if (project && state.selected !== project.id) setShared("trin.projects.v1", { ...state, selected: project.id });
        run("tab.select", { id: reply.tab });
        cx.notify();
      } catch {
        this.opening.cancel(agent.address, target);
        if (!this.closed) notify({ title: "Сессии", body: "Не удалось открыть сессию OpenCode.", level: "error" });
      }
    });
  }

  admittedBackground(cx, claimed, unresolvedMachines, now, openWakeAt) {
    this.backgroundAdmission ??= new BackgroundAdmission();
    const { rows, wakeAt: admissionWakeAt } = this.backgroundAdmission.update(this.activity.backgroundRows?.() ?? [],
      claimed, unresolvedMachines, now);
    const wakeAt = Math.min(admissionWakeAt, openWakeAt);
    if (cx && wakeAt !== this.backgroundWakeAt) {
      this.backgroundTimer?.cancel();
      this.backgroundWakeAt = wakeAt;
      this.backgroundTimer = Number.isFinite(wakeAt) ? cx.timer.after(Math.max(1, wakeAt - now), cx => {
        this.backgroundWakeAt = undefined;
        this.backgroundTimer = null;
        cx.notify();
      }) : null;
    }
    return rows;
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
      .when(!sorting, el => el.cursor_pointer())
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
        if (!active && source === id) select(agent, cx);
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
    const ps = panes();
    const all = new Map(terminals().map(terminal => [terminal.address, terminal]));
    const sp = spaces();
    const spaceById = new Map(sp.map(s => [s.id, s]));
    const tabs = new Map(sp.flatMap(s => s.tabs).map(tab => [tab.id, tab]));
    const aliases = new Map(), attached = new Map(), sessionIDs = new Map(), excluded = new Set();
    const owners = new Map();
    const openCodeByTab = new Map();
    const unresolvedMachines = new Set();
    const titles = {}, groups = new Map();
    const projects = shared("trin.projects.v1")?.projects ?? [];
    const directoryProjects = (machine, folder) => projects.filter(p => folder && p.folder === folder && (p.machine ?? null) === machine);
    const occupied = new Set();
    // `agents()` can also contain a child that was kept as a tab. Its
    // parent link is independent of whether the terminal has its own pane.
    for (const agent of attachedAgents(agents(), ps, all, sp)) {
      const terminal = all.get(agent.address);
      occupied.add(agent.tab);
      if (!terminal || terminal.parent) continue;
      const machine = spaceById.get(agent.space)?.machine ?? null;
      this.activity.checkPresentation?.(agent, terminal, machine);
      const sessionID = spaceById.has(agent.space) ? this.activity.boundSessionID(agent, terminal, false, machine) : null;
      if (!sessionID && isOpenCode(agent, terminal) && (agent.status === "working" || agent.status === "blocked")) {
        unresolvedMachines.add(machine);
      }
      const id = sessionID ? sessionAddress(machine, sessionID) : agent.address;
      if (isOpenCode(agent, terminal)) {
        if (!openCodeByTab.has(agent.tab)) openCodeByTab.set(agent.tab, []);
        openCodeByTab.get(agent.tab).push({ address: agent.address, identity: sessionID ? id : null });
      }
      const session = this.activity.session?.(agent, sessionID);
      if (session?.excluded) {
        excluded.add(id);
        continue;
      }
      if (sessionID) {
        sessionIDs.set(agent.address, sessionID);
        aliases.set(agent.address, id);
        if (!owners.has(id)) owners.set(id, new Set());
        owners.get(id).add(agent.address);
      }
      const display = session ?? (!sessionID ? this.activity.presentation?.(agent) : null);
      const title = typeof display?.sessionTitle === "string"
        ? [...display.sessionTitle.replace(/[\x00-\x1f\x7f-\x9f]/g, " ").trim()].slice(0, 256).join("") : "";
      const hint = !sessionID ? this.activity.presentationHint?.(agent) : null;
      const client = this.activity.explicitClient?.(agent);
      if (title) titles[agent.address] = { ...hint, ...client, sessionID: sessionID ?? display.sessionID, title, caption: agent.title ?? "",
        pid: terminal?.foreground?.pid ?? terminal?.pid ?? null, created: terminal?.created_ms ?? null,
        ...(!sessionID ? { presentationOnly: true, machine } : {}) };
      if (title && sessionID) titles[agent.address].proofCaption = this.activity.bindings?.get(agent.address)?.proofCaption;
      else if (!title && (hint || client)) titles[agent.address] = { ...hint, ...client, title: "" };
      const attention = agent.status === "done" || agent.status === "blocked";
      const hidden = Boolean(tabs.get(agent.tab)?.settled) && !attention;
      if (!groups.has(id)) groups.set(id, []);
      groups.get(id).push({ id, agent, hidden, folder: session?.sessionFolder });
    }
    for (const [id, group] of groups) {
      // Only a bound session's own directory can settle cross-project ownership.
      // Neither the focused pane, launch cwd nor a native tab caption proves it.
      const folders = [...new Set(group.map(r => r.folder).filter(Boolean))];
      const machine = spaceById.get(group[0].agent.space)?.machine ?? null;
      const matches = directoryProjects(machine, folders.length === 1 && folders[0]);
      const home = matches.length === 1 ? group.filter(r => r.agent.space === matches[0].space) : [];
      const preferredSpace = home.length ? home[0].agent.space : group.map(r => r.agent.space).sort()[0];
      const candidates = group.filter(r => r.agent.space === preferredSpace);
      const rank = r => Number(!r.hidden) * 16 + Number(Boolean(r.agent.focused)) * 4 + (ATTENTION[r.agent.status] ?? 0);
      const best = candidates.reduce((a, b) => rank(b) > rank(a) ? b : a);
      attached.set(id, best);
      if (home.length && group.some(r => r.agent.space !== best.agent.space)) {
        const sid = sessionIDs.get(best.agent.address);
        const name = titles[best.agent.address]?.title || group.map(r => titles[r.agent.address]?.title).find(Boolean);
        for (const { agent } of group) {
          const term = all.get(agent.address);
          if (!term) continue;
          const value = titles[agent.address] ??= {};
          Object.assign(value, { sessionID: sid, caption: agent.title ?? "",
            pid: term.foreground?.pid ?? term.pid, created: term.created_ms });
          value.proofCaption = this.activity.bindings?.get(agent.address)?.proofCaption;
          value.argsKey = processArgsKey(term);
          const proof = this.activity.resumeProof?.(agent, term, machine, sid);
          if (proof) value.resumeProof = proof;
          value.title ||= name || "";
          if (agent.space !== best.agent.space) value.mirrorOf = {
            sourceTab: agent.tab, sourceSpace: agent.space, terminal: best.agent.address, tab: best.agent.tab, space: best.agent.space };
        }
      }
    }
    // A terminal can drop out of agents() temporarily without going away.
    for (const address of all.keys()) {
      if (Object.hasOwn(titles, address)) continue;
      const client = this.activity.explicitClient?.({ address });
      if (client) titles[address] = { ...client, title: "" };
    }
    const titleSignature = JSON.stringify(titles);
    if (this.live && titleSignature !== this.titleSignature) {
      this.titleSignature = titleSignature;
      setShared(TITLES_KEY, titles);
    }
    const roots = [...attached.values()].map(row => row.agent);
    this.working = roots.some(a => a.status === "working");
    if (this.live !== false) this.updateBadge(roots);
    const spaceOf = a => {
      if (a.background) {
        const at = projects.filter(p => p.folder === a.folder && p.machine === a.machine);
        return at.length === 1 ? at[0] : { name: a.folder?.split(/[\\/]/).filter(Boolean).at(-1) || "OpenCode" };
      }
      const s = spaceById.get(a.space) ?? { id: a.space, name: a.space_name };
      const p = projects.find(p => p.space === a.space);
      return p ? { ...s, name: p.name, color: p.color, folder: p.folder, machine: p.machine } : s;
    };
    const focusAgent = (a, cx) => {
      if (a.background) {
        this.openBackground(a, cx);
        return;
      }
      const current = spaces().find(s => s.id === a.space)?.tabs.find(t => t.id === a.tab);
      if (!current) return;
      const expected = sessionIDs.get(a.address);
      if (expected) {
        const owner = this.opening.attached.get(sessionAddress(spaceById.get(a.space)?.machine, expected));
        if (owner?.address !== a.address) return;
      }
      if (a.pane ? !this.currentAttachment(a, expected) : (!current.settled || agents().some(agent => agent.tab === a.tab)
        || plainTabKey(current, panes(), new Map(terminals().map(t => [t.address, t]))) !== a.plainKey)) return;
      const state = shared("trin.projects.v1"), p = state?.projects?.find(p => p.space === a.space);
      if (p && state.selected !== p.id) setShared("trin.projects.v1", { ...state, selected: p.id });
      if (a.pane) run("pane.focus", { pane: a.pane });
      else run("tab.select", { id: a.tab });
      if (find(layout(), "session-body")?.active === "hidden-tabs-empty") {
        setShared("trin.sessions.reveal", { id: a.tab, at: Date.now() });
      }
    };
    // Track work time even when the compact rail doesn't show it.
    const now = Date.now();
    const details = a => {
      const sid = sessionIDs.get(a.address), own = this.activity.details(a, sid);
      if (own || !sid) return own;
      for (const row of groups.get(aliases.get(a.address)) ?? []) {
        if (row.agent.address === a.address) continue;
        const meta = this.activity.details(row.agent, sid);
        if (meta) return meta;
      }
      return null;
    };
    this.clock.sync(roots, details, now);
    const claimed = new Set([...attached.keys(), ...excluded]);
    this.opening ??= new BackgroundOpens();
    const pendingOpen = this.opening.reconcile(tabs, attached, openCodeByTab, all, now, ps);
    for (const id of pendingOpen.claimed) claimed.add(id);
    this.backgroundAdmission ??= new BackgroundAdmission();
    if (this.backgroundAdmission.remember(owners, all) && this.live) {
      localStorage.setItem(ATTACHED_KEY, JSON.stringify([...this.backgroundAdmission.seen]));
    }
    for (const agent of this.admittedBackground(cx, claimed, unresolvedMachines, now, pendingOpen.wakeAt)) {
      attached.set(agent.address, { id: agent.address, agent });
    }
    const rows = [...attached.values()].filter(item => !item.hidden);
    const hidden = [...attached.values()].filter(item => item.hidden);
    for (const space of sp) for (const tab of space.tabs) {
      if (!tab.settled || tab.status === "blocked" || tab.status === "done" || occupied.has(tab.id)) continue;
      const plainKey = plainTabKey(tab, ps, all);
      if (plainKey) hidden.push({ agent: { address: `hidden-tab:${tab.id}`, space: space.id, tab: tab.id,
        plainKey, title: tab.name, status: tab.status ?? "idle" } });
    }
    this.renderOrder = [...new Set(this.order.map(id => aliases.get(id) ?? id))];
    this.rows = orderedSessions(rows, this.renderOrder);
    if (this.drag && (!rows.some(row => row.id === this.drag.id)
      || (this.drag.over && !rows.some(row => row.id === this.drag.over)))) this.drag = null;
    if (this.live !== false) setAgentOrder(this.rows.filter(row => !row.agent.background).map(row => row.agent.address));

    const label = (a) => sessionName(tabs.get(a.tab), a, titles[a.address]?.title);

    const rowHeight = collapsed ? RAIL_ROW_HEIGHT : WIDE_ROW_HEIGHT;
    const row = (a, bottomPadding = 0) => {
      const pic = spaceAvatar(spaceOf(a), t);
      const sid = sessionIDs.get(a.address);
      const meta = a.background ? a.meta : details(a);
      const backgroundHint = a.background ? `Фоновая сессия · ${a.machine ?? "Этот компьютер"}` : null;
      const ready = a.status === "done";
      if (collapsed) {
        const needsYou = a.status === "blocked";
        const attention = ready || needsYou;
        const signal = needsYou ? t.blocked : t.done;
        const description = [label(a), spaceOf(a).name || a.space_name,
          `${a.program?.name || "Agent"} · ${a.status}`,
          ready ? "Готово — откройте сессию, чтобы посмотреть результат" : needsYou ? "Нужно ваше действие — откройте сессию" : null,
           modelDescription(meta),
          backgroundHint ?? a.address, this.activity?.notice(a, sid)].filter(Boolean).join("\n");
        return h_flex()
          .id(`ag-${a.address}`)
          .h(rowHeight + bottomPadding)
          .when(bottomPadding > 0, el => el.pb(bottomPadding))
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
        .h(rowHeight + bottomPadding)
        .pt(6)
        .pb(6 + bottomPadding)
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

    const hiddenHeight = HIDDEN_HEADER_HEIGHT + (this.hiddenOpen
      ? Math.min(hidden.length, HIDDEN_VISIBLE_ROWS) * (rowHeight + HIDDEN_ROW_BOTTOM_PADDING) : 0);
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
          el.child(div().px(4).text_size(12).text_color(t.muted).child(hidden.length ? "Нет открытых сессий." : "Нет сессий со статусом.")))
        .children(this.rows.map(item => this.sortable(row(item.agent), item, label(item.agent), collapsed, t, focusAgent))))
      // The scrollbar wrapper has no intrinsic height. Reserve space for
      // its viewport explicitly, leaving room for the main sessions list.
      .when(hidden.length > 0, el => el.child(v_flex().id("hidden-sessions").w_full().flex_none()
          .h(hiddenHeight).min_h(HIDDEN_HEADER_HEIGHT).when(this.hiddenOpen, section => section.max_h("45%"))
          .child(h_flex().id("hidden-sessions-toggle").w_full().h(HIDDEN_HEADER_HEIGHT).px(collapsed ? 2 : 14).gap(4).items_center()
            .flex_none().when(collapsed, h => h.justify_center()).border_t(1).border_color(t.border)
            .text_color(t.muted).text_size(11).cursor_pointer().hover(h => h.bg(t.hover))
            .tooltip(`Скрытые сессии: ${hidden.length}`).accessibility_label(`Скрытые сессии: ${hidden.length}`)
            .on_click((_e, cx) => {
              this.hiddenOpen = !this.hiddenOpen;
              localStorage.setItem("sessions.hidden-open", String(this.hiddenOpen));
              cx.notify();
            })
            .child(svg(`mods/workspace-chrome/chevron-${this.hiddenOpen ? "down" : "right"}.svg`).size(11).flex_none())
            .when(!collapsed, h => h.child(div().flex_1().truncate().child("Скрытые")))
            .child(div().child(String(hidden.length))))
          .when(Boolean(this.hiddenOpen), section => section.child(v_flex().id("hidden-sessions-list").flex_1().min_h(0)
            .px(collapsed ? 0 : 8).overflow_y_scrollbar()
            .children(hidden.map(({ agent }) => row(agent, HIDDEN_ROW_BOTTOM_PADDING).cursor_pointer()
              .accessibility_label(`Вернуть: ${label(agent)}`).on_click((_e, cx) => focusAgent(agent, cx))))))))
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
