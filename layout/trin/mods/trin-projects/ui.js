// Operate mode: retain the existing native Spaces typography, avatars and rhythm.
// Projects keep their name/root; empty and offline are visible, not hidden sessions.
import { div, svg } from "gpui-kit";
import { v_flex, h_flex, InputState, Input } from "gpui-base";
import { props, closeOverlay, theme, knownAgents, run, agents, window, keys } from "herdr";
import { launchLocked } from "./data.js";
import { read, write, current, live, ready, command, overlay, target } from "./store.js";
import { initProjectForm, renderProjectForm } from "./form.js";

const button = (id, text, action, enabled = true) => div().id(id).h(32).px(12).flex_none().flex().items_center().rounded(6)
  .bg(theme().raised).text_color(enabled ? theme().foreground : theme().dim).cursor_pointer()
  .when(enabled, el => el.hover(s => s.bg(theme().hover))).on_click((_e, cx) => { if (enabled) action(cx); }).child(text);
const field = (input, label) => v_flex().gap(6).child(div().text_size(11.5).text_color(theme().muted).child(label))
  .child(Input.new(input).h(36).px(10).rounded(7).border_1().border_color(theme().input).bg(theme().background).text_color(theme().foreground));

export function renderStatus() {
  const t = theme(), p = current(), s = live(p), list = agents(), needs = list.filter(a => a.status === "blocked").length;
  const mac = window()?.platform === "macos";
  const mods = { cmd: mac ? "⌘" : "Ctrl+", ctrl: mac ? "⌃" : "Ctrl+", shift: mac ? "⇧" : "Shift+", alt: mac ? "⌥" : "Alt+" };
  const binding = keys().find(k => k.section === "global" && k.action === "app.palette");
  const hint = binding ? `${binding.keys.split("-").map(p => mods[p.toLowerCase()] ?? p.toUpperCase()).join("")}  commands` : "commands";
  const label = (icon, text) => h_flex().gap(6).items_center().child(svg(`mods/workspace-chrome/${icon}.svg`).size(13)).child(text);
  return h_flex().size_full().min_w(0).px(14).gap(16).items_center().bg(t.chrome).border_t(1).border_color(t.border).text_size(11.8).text_color(t.muted)
    .child(label(p?.machine ? "server" : "monitor", p?.machine ?? "This computer"))
    .child(div().min_w(0).truncate().child(p ? `${p.name} · ${s?.tabs?.length ?? 0} сессий` : "Projects"))
    .child(h_flex().gap(6).items_center().child(div().size(6).rounded(3).bg(t.dim)).child(`${list.length} agents`)
      .when(needs > 0, el => el.child(div().text_color(t.blocked).child(`· ${needs} needs you`))))
    .child(div().flex_1()).child(div().id("palette").flex_none().text_color(t.dim).on_click(() => run("app.palette")).child(hint));
}

export function initSurface(view, cx) {
  const surface = props()?.surface;
  view.handle = cx.focus_handle();
  view.error = "";
  view.at = 0;
  view.closed = false;
  if (surface === "form") {
    initProjectForm(view, cx);
  } else if (surface === "picker") {
    view.filter = InputState.new({ placeholder: "Найти проект…" });
    view.at = 0;
    view.filter.on("change", (_e, cx) => { view.at = 0; cx.notify(); });
    view.filter.on("submit", () => chooseFiltered(view));
    view.filter.focus();
  } else if (surface === "agents") {
    view.agents = [];
    const p = target();
    if (p && ready(p)) cx.spawn(async cx => {
      try { const list = await knownAgents({ on: p.machine }); if (!view.closed) view.agents = list.filter(a => a.installed); }
      catch { if (!view.closed) view.error = "Не удалось загрузить агентов. Попробуйте ещё раз."; }
      if (!view.closed) { view.loaded = true; cx.notify(); }
    });
  } else if (surface !== "empty") view.handle.focus();
}

const filtered = view => read().projects.filter(p => `${p.name} ${p.folder} ${p.machine ?? "This computer"}`.toLowerCase().includes(view.filter.value().toLowerCase()));
function chooseFiltered(view) {
  const p = filtered(view)[view.at];
  if (p) { closeOverlay(props().overlay); command("select", p.id); }
}

export function renderSurface(view, cx) {
  const t = theme(), surface = props()?.surface;
  if (surface === "form") return renderProjectForm(view);
  if (surface === "empty") {
    const p = current(), available = p && ready(p);
    return v_flex().id("project-empty").size_full().min_w(0).min_h(0).items_center().justify_center().p(24).gap(12).bg(t.canvas).text_color(t.foreground)
      .child(div().text_size(18).font_semibold().max_w("100%").truncate().child(p?.name ?? "Projects"))
      .child(div().text_size(12).text_color(t.muted).max_w("100%").truncate().tooltip(p?.folder ?? "").child(p?.folder ?? "Добавьте проект, чтобы начать работу"))
      .child(div().text_size(12).text_color(t.muted).child(!p ? "" : !available ? `${p.machine ?? "This computer"}: машина недоступна или подключается`
        : p.pending ? launchLocked(p) ? "Ожидаем подтверждение запуска…" : "Результат запуска неизвестен. Повторная попытка проверит состояние машины." : "В этом проекте пока нет сессий"))
      .child(button("project-start", launchLocked(p) ? "Запуск…" : p ? "Новая сессия" : "Добавить проект", () => p ? command("new-tab") : overlay("form"), !p || available && !launchLocked(p)))
      .when(Boolean(p), el => el.child(button("project-edit-empty", "Настройки проекта", () => overlay("form", { project: p.id }))));
  }
  const root = v_flex().id(`projects-${surface}`).track_focus(view.handle).w_full().min_h(0).p(18).gap(12).bg(t.popover).text_color(t.foreground).text_size(12.8)
    .on_key_down((e, cx) => {
      if (e.key === "escape") { closeOverlay(props().overlay); cx.stop_propagation(); }
      if (surface === "picker" && (e.key === "down" || e.key === "up")) {
        view.at = Math.max(0, Math.min(filtered(view).length - 1, view.at + (e.key === "down" ? 1 : -1))); cx.notify(); cx.stop_propagation();
      }
      if (surface === "menu") {
        const list = view.menuActions ?? [];
        if (e.key === "down" || e.key === "up") { view.at = (view.at + (e.key === "down" ? 1 : -1) + list.length) % list.length; cx.notify(); cx.stop_propagation(); }
        else if (e.key === "enter") { list[view.at]?.(); cx.stop_propagation(); }
      }
    });
  if (surface === "picker") {
    const list = filtered(view);
    return root.h_full().child(field(view.filter, "Projects"))
      .child(v_flex().flex_1().min_h(0).overflow_y_scrollbar().children(list.map((p, i) => v_flex().id(`pick-${p.id}`).flex_none().px(8).py(8).rounded(6)
        .when(i === view.at, el => el.bg(t.selected)).hover(el => el.bg(t.hover)).cursor_pointer()
        .on_click(() => { closeOverlay(props().overlay); command("select", p.id); })
        .child(div().truncate().child(p.name)).child(div().text_size(11).text_color(t.muted).truncate().child(`${p.machine ?? "This computer"} · ${p.folder}`))))
        .when(!list.length, el => el.child(div().text_color(t.muted).child("Проекты не найдены"))))
      .child(button("project-picker-add", "Добавить проект", () => { closeOverlay(props().overlay); overlay("form"); }));
  }
  const p = target();
  if (!p) return root.child("Проект больше не существует");
  const s = live(p), available = ready(p);
  if (surface === "agents") return root.h_full().child(div().text_size(15).font_semibold().truncate().child(`Новая сессия · ${p.name}`))
    .child(button("project-shell", "Терминал", () => { closeOverlay(props().overlay); command("new-tab", p.id); }, available))
    .child(v_flex().flex_1().min_h(0).overflow_y_scrollbar().gap(6).children((view.agents ?? []).map(a => button(`project-agent-${a.key}`, a.name, () => {
      closeOverlay(props().overlay); command("agent", `${p.id}|${a.key}`);
    }, available))))
    .child(div().text_size(11.5).text_color(t.muted).child(view.error || (!available ? "Машина недоступна" : !view.loaded ? "Загрузка агентов…" : !view.agents.length ? "Установленные агенты не найдены" : p.folder)));
  view.menuActions = [];
  const item = (id, text, action, enabled = true) => {
    const i = view.menuActions.length;
    const choose = () => { if (enabled) { closeOverlay(props().overlay); action(); } };
    view.menuActions.push(choose);
    return button(id, text, choose, enabled).w_full().when(i === view.at, el => el.bg(t.selected));
  };
  return root.p(5).gap(3)
    .child(item("project-menu-new", "Новая сессия…", () => overlay("agents", { project: p.id }), available && !launchLocked(p)))
    .child(item("project-menu-edit", "Настройки проекта…", () => overlay("form", { project: p.id })))
    .child(item("project-menu-close", "Закрыть все сессии…", () => run("space.close", { id: s.id }), available && Boolean(s)))
    .child(div().h(1).my(4).bg(t.border))
    .child(item("project-menu-remove", "Удалить пустой проект…", () => command("confirm-remove", p.id), available && !s && !launchLocked(p)))
    .child(div().px(10).text_size(11).text_color(t.muted).child(s ? "Закрытие сессий сохраняет проект" : !available ? "Проект сохранён · машина недоступна" : "Нет сессий"));
}
