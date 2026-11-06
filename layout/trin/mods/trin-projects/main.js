// Projects preserve the incumbent Spaces sidebar: same native rows, machine
// headers, avatars and rail. Only identity/lifetime changes; cwd is session data.
import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { spaces, agents, machines, focused, run, nextCommand, exec, theme, shared, setShared, props, layout, notify, knownAgents, openOverlay } from "herdr";
import { MARK, adopt, liveSpace, machineReady, projectForSpace, selectedProject, updateProject, moveProject, removeProject, launchArgs, launchLocked } from "./data.js";
import { seeds } from "./migration.js";
import { read, write, command, overlay } from "./store.js";
import { initSurface, renderSurface, renderStatus } from "./ui.js";
import { disposeRemoteFolderPicker } from "./remote-picker.js";

function find(node, id) {
  if (node?.id === id) return node;
  for (const child of node?.children ?? []) { const found = find(child, id); if (found) return found; }
  return null;
}
const narrow = () => (find(layout(), "sidebar")?.size?.px ?? Infinity) < 160;
const focusKey = () => { const f = focused(); return JSON.stringify([f?.space, f?.tab, f?.pane]); };

export default class Projects extends View {
  init(_props, cx) {
    this.closed = false;
    const surface = props()?.surface;
    if (surface && surface !== "status") { initSurface(this, cx); return; }
    this.pressed = null; this.over = null;
    this.nativeFocus = focusKey();
    this.folded = new Set(JSON.parse(localStorage.getItem("machines.folded") ?? "[]"));
    // The always-visible status strip owns actions, not the hideable sidebar.
    // Sidebar and overlays are presentation only, avoiding duplicate consumers.
    if (surface !== "status") return;
    this.sync(cx);
    // A single owner processes project actions; overlays/empty blocks never do.
    cx.spawn(async cx => {
      for (;;) {
        const message = await nextCommand();
        if (this.closed) return;
        try { await this.act(message, cx); } catch (e) { if (!this.closed) notify({ title: "Projects", body: String(e.message ?? e), level: "error" }); }
        if (!this.closed) cx.notify();
      }
    });
  }

  close() {
    this.closed = true;
    disposeRemoteFolderPicker(this);
  }

  sync(cx) {
    let state = read();
    let recovered = false;
    // While space.new is in flight, the new space may be visible before its
    // acknowledged ID arrives. Do not accidentally import it as another project.
    if (!this.launching) {
      const next = adopt(state, spaces(), seeds, shared("space.colors") ?? {});
      recovered = next.projects.some(p => state.projects.some(old => old.id === p.id && old.pending && !p.pending && old.space !== p.space));
      if (next !== state) { write(next); state = next; }
    }
    const key = focusKey();
    if (key !== this.nativeFocus && !this.launching && !recovered) {
      const active = spaces().find(s => s.active);
      const p = projectForSpace(state, active?.id);
      if (p && state.selected !== p.id) { state = { ...state, selected: p.id }; write(state); }
    }
    this.nativeFocus = key;
    this.syncWorkspace(state);
    this.nativeFocus = focusKey();
    return state;
  }

  syncWorkspace(state) {
    const p = selectedProject(state), s = liveSpace(p, spaces());
    // On startup/recovery the registry may select B while native focus is A.
    // {tab:"active"} must show the same Space as project Tabs and panel context.
    if (s && !s.active && machineReady(p, machines())) run("space.select", { id: s.id });
    const workspace = find(layout(), "project-workspace");
    const child = s ? "main" : "project-empty";
    if (workspace && workspace.active !== child) run("layout.select", { stack: "project-workspace", child });
    const panel = find(layout(), "panel");
    if (!s && panel && !panel.hidden) run("layout.hide", { id: "panel" });
  }

  select(id) {
    const state = read(), p = state.projects.find(p => p.id === id);
    if (!p) return;
    write({ ...state, selected: id });
    const s = liveSpace(p, spaces());
    if (s) run("space.select", { id: s.id });
    this.nativeFocus = focusKey();
    this.syncWorkspace(read());
  }

  async newTab(id, extra = {}) {
    if (this.closed) return;
    let state = read();
    const p = state.projects.find(p => p.id === (id ?? state.selected));
    if (!p) { overlay("form"); return; }
    if (!machineReady(p, machines())) throw new Error(`${p.machine ?? "This computer"}: дождитесь подключения машины.`);
    const args = launchArgs(p, { ...extra, ...(extra.name ? { tab: extra.name } : {}) });
    const s = liveSpace(p, spaces());
    if (s) {
      this.select(p.id);
      run("tab.new", { ...extra, space: s.id, folder: p.folder });
      this.nativeFocus = focusKey();
      return;
    }
    if (launchLocked(p)) throw new Error("Предыдущий запуск ещё завершается. Попробуйте через несколько секунд.");
    if (this.launching) throw new Error("Создание сессии уже выполняется.");
    this.select(p.id);
    this.launching = p.id;
    write(updateProject(read(), p.id, { pending: { at: Date.now(), name: p.name, folder: p.folder, before: spaces().map(s => s.id) } }));
    try {
      // run() has no result. The supported CLI provides the actual created IDs
      // (and startup errors), avoiding ID guessing, polling and dummy terminals.
      const r = await exec("herdr-app", ["do", "space.new", JSON.stringify(args)]);
      const result = JSON.parse(r.stdout || "{}");
      if (r.code !== 0 || result.error || typeof result.space !== "string") throw new Error(result.error || r.stderr || "Не удалось создать сессию.");
      if (this.closed) return;
      state = read();
      if (!state.projects.some(k => k.id === p.id)) throw new Error("Проект удалён во время запуска; созданный Space оставлен доступным.");
      write(updateProject(state, p.id, { space: result.space, pending: null }));
      // A user can select another project while CLI startup is pending.
      this.select(read().selected);
    } catch (e) {
      // A CLI failure/timeout does not prove that native creation failed.
      // Keep the request for late recovery; a deliberate retry rechecks a ready
      // machine's current layout in act()/newTab before replacing the request.
      if (!this.closed) {
        const pending = read().projects.find(k => k.id === p.id)?.pending;
        if (pending) write(updateProject(read(), p.id, { pending: { ...pending, failed: true } }));
      }
      throw e;
    } finally { this.launching = null; }
  }

  startLaunch(id, agentKey, cx) {
    if (this.closed) return;
    const p = read().projects.find(p => p.id === (id ?? read().selected));
    if (!p) { overlay("form"); return; }
    if (this.jobBusy) throw new Error("Предыдущая сессия ещё запускается. Дождитесь результата.");
    const intended = p.id;
    this.select(intended);
    this.jobBusy = true;
    cx.spawn(async cx => {
      try {
        let extra = {};
        if (agentKey) {
          if (!machineReady(p, machines())) throw new Error("Машина недоступна.");
          const list = await knownAgents({ on: p.machine });
          if (this.closed || read().selected !== intended) return;
          const agent = list.find(a => a.key === agentKey && a.installed);
          if (!agent) throw new Error("Агент больше не установлен на этой машине.");
          extra = { argv: agent.run, name: agent.name };
        }
        if (this.closed || !read().projects.some(p => p.id === intended)) return;
        await this.newTab(intended, extra);
      } catch (e) { if (!this.closed) notify({ title: "Projects", body: String(e.message ?? e), level: "error" }); }
      finally {
        this.jobBusy = false;
        if (!this.closed) { this.sync(cx); cx.notify(); }
      }
    });
  }

  async act(message, cx) {
    const separator = message.indexOf("--"), action = separator < 0 ? message : message.slice(0, separator);
    const id = separator < 0 ? null : decodeURIComponent(message.slice(separator + 2));
    const state = this.sync(cx);
    if (action === "select") this.select(id);
    else if (action === "new-tab") this.startLaunch(id, null, cx);
    else if (action === "new") overlay("form");
    else if (action === "browse") {
      const machine = id ?? selectedProject(state)?.machine;
      if (!machine || !machines().some(m => m.name === machine)) throw new Error("Укажите добавленную удалённую машину.");
      overlay("form", { machine, browse: true });
    }
    else if (action === "picker") overlay("picker");
    else if (action === "diagnostics") setShared("trin.projects.diagnostics", { machines: machines(), selected: state.selected, launching: this.launching ?? null, busy: this.jobBusy ?? false, surface: props()?.surface });
    else if (action === "edit") { const p = selectedProject(state); if (p) overlay("form", { project: p.id }); }
    else if (action === "agent-next" || action === "agent-prev") {
      run(action === "agent-next" ? "agent.next" : "agent.prev");
      const p = projectForSpace(read(), focused()?.space);
      if (p) this.select(p.id);
    } else if (action === "tab-index" || action === "tab-next" || action === "tab-prev" || action === "close-tab" || action === "close-pane" || action === "split-right" || action === "split-down" || action === "zoom" || action === "rename-tab" || action === "pane-go" || action === "pane-swap") {
      const p = selectedProject(state), s = liveSpace(p, spaces());
      // An empty project leaves the native focus behind in another Space.
      // Never let ordinary shortcuts close/split that unrelated terminal.
      if (!s || !machineReady(p, machines())) return;
      this.select(p.id);
      if (action === "tab-index") run("tab.select", { index: Number(id) });
      else if (action === "tab-next" || action === "tab-prev") run(action === "tab-next" ? "tab.next" : "tab.prev");
      else if (action === "close-tab") run("tab.close", { id: s.tabs.find(t => t.active)?.id });
      else if (action === "rename-tab") run("tab.rename", { id: s.tabs.find(t => t.active)?.id });
      else if (action === "pane-go" || action === "pane-swap") run(action === "pane-go" ? "pane.go" : "pane.swap", { direction: id });
      else if (action === "split-right" || action === "split-down") run("pane.split", { direction: action === "split-right" ? "right" : "down" });
      else run(action === "zoom" ? "pane.zoom" : "pane.close");
      this.nativeFocus = focusKey();
    }
    else if (action === "agent") {
      const split = id.lastIndexOf("|"), projectID = id.slice(0, split), agentKey = id.slice(split + 1);
      this.startLaunch(projectID, agentKey, cx);
    } else if (action === "next" || action === "prev" || action === "index") {
      const list = this.ordered(state), at = list.findIndex(p => p.id === state.selected);
      const p = action === "index" ? list[Number(id) - 1] : list[(at + (action === "next" ? 1 : -1) + list.length) % list.length];
      if (p) this.select(p.id);
    } else if (action === "confirm-remove" || action === "remove") {
      const p = state.projects.find(p => p.id === id);
      if (!p || launchLocked(p) || this.launching === id || !machineReady(p, machines()) || liveSpace(p, spaces())) throw new Error("Сначала закройте сессии проекта и дождитесь подключения машины.");
      if (action === "confirm-remove") {
        // Confirmation delegates back to this owner and rechecks live state.
        openOverlay({ mod: "confirm", kind: "dialog", width: 440, height: 210, props: { title: `Удалить проект «${p.name}»?`,
          detail: "Директория и файлы останутся на машине. Удаляется только запись в Projects." + (p.pending ? "\nРезультат прежнего запуска неизвестен: если он завершится позднее, Space появится отдельно." : ""), command: { name: `trin-projects.remove--${encodeURIComponent(id)}` } } });
      } else { write(removeProject(state, id)); this.select(read().selected); }
    }
    this.sync(cx);
  }

  ordered(state) {
    const names = [...new Set(state.projects.map(p => p.machine).filter(Boolean))].sort();
    return [null, ...names].flatMap(machine => state.projects.filter(p => p.machine === machine));
  }

  row(p, t, collapsed) {
    const state = read(), on = state.selected === p.id, fill = p.color?.startsWith("#") ? p.color : MARK[p.color] ?? t.ansi6;
    const s = liveSpace(p, spaces()), available = machineReady(p, machines());
    const list = agents().filter(a => a.space === s?.id), rank = { blocked: 4, done: 3, working: 2, idle: 1 };
    const status = list.reduce((best, a) => (rank[a.status] ?? 0) > (rank[best] ?? 0) ? a.status : best, null);
    const working = list.some(a => a.status === "working");
    const title = `${p.name}\n${p.machine ?? "This computer"}\n${p.folder}\n${!available ? "Машина недоступна / подключается" : `${s?.tabs?.length ?? 0} сессий`}${p.pending ? "\nРезультат предыдущего запуска ещё не подтверждён" : ""}`;
    const avatar = size => h_flex().size(size).flex_none().rounded(5).bg(fill).items_center().justify_center().font_bold().text_color("#ffffff").text_size(collapsed ? 12 : 10.5).child([...p.name][0]?.toUpperCase() ?? "?");
    if (collapsed) return h_flex().id(`project-${p.id}`).h(36).flex_none().justify_center().items_center().cursor_pointer().tooltip(title)
      .on_click(() => this.select(p.id)).on_mouse_down("right", () => overlay("menu", { project: p.id }))
      .child(h_flex().size(32).flex_none().rounded(7).items_center().justify_center()
        .when(on, el => el.bg(t.selected).border(1).border_color(t.accent)).when(!on, el => el.hover(s => s.bg(t.hover)))
        .child(avatar(22).when(!available, el => el.opacity(0.55))));
    return h_flex().id(`project-${p.id}`).h(30).px(8).gap(9).flex_none().items_center().rounded(8).tooltip(title)
      .when(on, el => el.bg(t.selected)).when(!on, el => el.hover(s => s.bg(t.hover)))
      .when(this.pressed === p.id && this.over !== p.id, el => el.opacity(0.5))
      .on_mouse_down("left", () => { this.pressed = p.id; this.over = p.id; })
      .on_mouse_move((_e, cx) => {
        const source = read().projects.find(k => k.id === this.pressed);
        const over = source?.machine === p.machine ? p.id : null;
        if (this.pressed && over !== this.over) { this.over = over; cx.notify(); }
      })
      .on_mouse_up("left", (_e, cx) => {
        const from = this.pressed, over = this.over; this.pressed = null; this.over = null;
        if (from === p.id) this.select(p.id);
        else if (over === p.id) write(moveProject(read(), from, p.id));
        cx.notify();
      })
      .on_mouse_down("right", () => overlay("menu", { project: p.id }))
      .child(avatar(18).when(!available, el => el.opacity(0.55)))
      .child(div().flex_1().min_w(0).truncate().text_color(t.foreground).child(p.name))
      .when(working, el => el.child(svg("mods/workspace-chrome/spinner.svg").size(12).flex_none().text_color(t.working).spin(900)))
      .child(div().flex_none().min_w(18).h(18).px(5).rounded(5).bg(status && status !== "idle" ? t[status] + "26" : t.raised)
        .text_size(10.5).text_color(!available ? t.dim : t[status] ?? t.muted).flex().items_center().justify_center()
        .child(p.pending ? launchLocked(p) ? "…" : "?" : !available ? "·" : String(s?.tabs?.length ?? 0)));
  }

  render(cx) {
    if (props()?.surface === "status") {
      try { this.sync(cx); return renderStatus(); }
      catch (e) { return div().p(4).text_color(theme().blocked).child(String(e.message ?? e)); }
    }
    if (props()?.surface) return renderSurface(this, cx);
    const t = theme(), collapsed = narrow();
    let state;
    try { state = read(); } catch (e) { return div().p(12).text_color(t.blocked).child(String(e.message ?? e)); }
    const rows = this.ordered(state);
    const remote = [...new Set([...rows.map(p => p.machine), ...machines().map(m => m.name)].filter(Boolean))].sort();
    const groups = collapsed ? rows.map(p => this.row(p, t, true)) : [null, ...remote].flatMap(machine => {
      const mine = rows.filter(p => p.machine === machine), folded = this.folded?.has(machine);
      const available = machines().find(m => (m.name ?? null) === machine)?.layout;
      if (!remote.length) return mine.map(p => this.row(p, t, false));
      return [h_flex().id(`project-machine-${machine ?? "local"}`).h(26).px(8).gap(7).flex_none().items_center().text_size(11).text_color(t.dim).cursor_pointer().hover(s => s.bg(t.hover))
        .on_click((_e, cx) => { folded ? this.folded.delete(machine) : this.folded.add(machine); localStorage.setItem("machines.folded", JSON.stringify([...this.folded])); cx.notify(); })
        .child(svg(`mods/workspace-chrome/${machine ? "server" : "monitor"}.svg`).size(13).flex_none())
        .child(div().flex_1().min_w(0).truncate().child(machine ?? "This computer"))
        .child(div().id(`project-add-${machine ?? "local"}`).px(4).rounded(4).tooltip("Добавить проект на этой машине").hover(s => s.bg(t.hover).text_color(t.foreground))
          .on_click((_e, cx) => { cx.stop_propagation(); overlay("form", { machine }); }).child("+"))
        .child(div().size(6).rounded(3).flex_none().bg(available === "ready" ? t.ansi2 : t.dim))
        .child(svg(`mods/workspace-chrome/chevron-${folded ? "right" : "down"}.svg`).size(11).flex_none()),
      ...(folded ? [] : mine.map(p => this.row(p, t, false)))];
    });
    return v_flex().size_full().min_h(0).bg(t.chrome).text_size(13)
      .child(h_flex().h(31).pl(collapsed ? 0 : 16).pr(collapsed ? 0 : 10).items_center().text_size(11.5).text_color(t.dim)
        .when(collapsed, el => el.justify_center()).when(!collapsed, el => el.justify_between().child("Projects"))
        .child(div().id("project-new").size(26).flex_none().flex().items_center().justify_center().rounded(6).tooltip("Новый проект")
          .hover(s => s.bg(t.hover).text_color(t.foreground)).on_click(() => overlay("form")).child(svg("mods/workspace-chrome/plus.svg").size(14))))
      .child(v_flex().flex_1().min_h(0).px(collapsed ? 0 : 8).pb(14).overflow_y_scrollbar().when(!collapsed, el => el.mode("always"))
        .on_mouse_up("left", (_e, cx) => { this.pressed = null; this.over = null; cx.notify(); }).children(groups));
  }
}
// Modified derivative of Herdr spaces (Apache-2.0): persistent project controller/UI.
// Original modifications: MIT. See repository THIRD_PARTY.md.
