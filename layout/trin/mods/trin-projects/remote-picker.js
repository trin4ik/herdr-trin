// A folder-only remote picker, inside the existing form: cancel preserves drafts.
import { div, svg } from "gpui-kit";
import { v_flex, h_flex, Button, InputState, Input, v_virtual_list, Scrollbar, VirtualListScrollHandle } from "gpui-base";
import { FolderBrowser, folderParent, folderChild } from "./folder-browser.js";
import { createDirectoryReader, directoryMessage } from "./remote-directory.js";
import { directoryName } from "./data.js";

const pickerButton = (id, text, action, t, enabled = true, primary = false) => Button.new(id).h(32).px(10).flex_none().rounded(6)
  .disabled(!enabled).bg(primary ? t.solid : t.raised).text_color(!enabled ? t.dim : primary ? t.solid_foreground : t.foreground)
  .text_size(12).cursor_pointer().when(enabled, el => el.hover(s => s.opacity(0.88))).on_click((_e, cx) => { if (enabled) action(cx); }).child(text);

const iconButton = (id, icon, label, action, t, enabled = true) => Button.new(id)
  .size(30).flex_none().flex().items_center().justify_center().rounded(6)
  .disabled(!enabled).bg(t.raised).text_color(enabled ? t.foreground : t.dim)
  .tooltip(label).accessibility_label(label).cursor_pointer()
  .when(enabled, el => el.hover(s => s.bg(t.hover)))
  .on_click((_e, cx) => { if (enabled) action(cx); })
  .child(svg(`assets/trin-projects/${icon}.svg`).size(16).flex_none());

function setAddress(p, value) {
  p.resetting = true;
  p.path.set_value(value);
  p.resetting = false;
}

const confirmedAddress = (p, snapshot) => snapshot && (p.path.value() === snapshot.path
  || (!p.model.query && p.path.value().endsWith("/") && p.location === snapshot.path));

function filterRemoteFolders(view, cx) {
  const p = view.remotePicker;
  if (!p || view.closed) return;
  p.debounce?.cancel();
  p.model.cancel(); // Invalidate replies immediately, not after the debounce.
  const value = p.path.value(), slash = value.lastIndexOf("/");
  p.location = slash < 0 ? p.model.snapshot?.path ?? "~" : value.slice(0, slash) || "/";
  p.model.query = value.slice(slash + 1);
  p.model.error = "";
  // Keep the confirmed list interactive during the typing pause. Only the
  // actual remote request enters loading; stale replies are still invalidated.
  p.debounce = cx.timer.after(350, cx => {
    if (view.closed || view.remotePicker !== p) return;
    p.debounce = null;
    navigateRemoteFolder(view, p.location, cx, "search");
  });
  cx.notify();
}

export function openRemoteFolderPicker(view, cx) {
  if (view.closed || !view.machine) return;
  if (!view.remoteBrowser) view.remoteBrowser = new FolderBrowser(createDirectoryReader(), () => !view.closed);
  const model = view.remoteBrowser;
  model.reset();
  const picker = { model, machine: view.machine, original: view.folder.value(), token: ++view.pickVersion,
    path: InputState.new({ value: view.folder.value() || "~", placeholder: "Полный путь или ~/директория" }),
    debounce: null, resetting: false, location: view.folder.value() || "~",
    pathEpoch: 0, at: 0, focus: cx.focus_handle(), listFocus: cx.focus_handle(), scroll: VirtualListScrollHandle.new() };
  view.remotePicker = picker;
  picker.path.on("change", (_e, cx) => {
    if (!view.closed && view.remotePicker === picker) {
      picker.pathEpoch++;
      if (!picker.resetting) filterRemoteFolders(view, cx);
      cx.notify();
    }
  });
  picker.path.on("submit", (_e, cx) => { if (!view.closed && view.remotePicker === picker) navigateRemoteFolder(view, picker.path.value(), cx); });
  model.onSnapshot = (snapshot, meta) => {
    if (view.remotePicker !== picker || meta.token !== picker.token || view.machine !== picker.machine) return;
    picker.location = snapshot.path;
    if (!meta.more) { picker.at = 0; picker.scroll.scroll_to_item(0); }
    if (picker.pathEpoch === meta.epoch && !meta.preservePath) {
      setAddress(picker, snapshot.path);
      if (!meta.search && !meta.more) picker.listFocus.focus();
    }
  };
  model.onChoose = (snapshot, meta, cx) => {
    if (view.remotePicker !== picker || view.machine !== picker.machine || view.pickVersion !== picker.token || meta.token !== picker.token || picker.pathEpoch !== meta.epoch) return;
    if (view.folder.value() !== picker.original) { model.error = "Путь в форме уже изменён. Отмените выбор и откройте его заново."; return; }
    view.folder.set_value(snapshot.path);
    if (!view.name.value().trim()) view.name.set_value(directoryName(snapshot.path));
    view.folderDrafts[JSON.stringify(picker.machine)] = snapshot.path;
    delete view.fieldErrors.folder;
    view.error = "";
    cancelRemoteFolderPicker(view, cx);
  };
  picker.focus.focus();
  navigateRemoteFolder(view, picker.path.value(), cx);
}

export function disposeRemoteFolderPicker(view) {
  const picker = view.remotePicker;
  view.remoteBrowser?.cancel();
  if (view.remoteBrowser) {
    view.remoteBrowser.onSnapshot = () => {};
    view.remoteBrowser.onChoose = () => {};
    view.remoteBrowser.onError = () => {};
  }
  view.remotePicker = null;
  picker?.debounce?.cancel();
  if (picker) for (const handle of [picker.scroll, picker.path, picker.focus, picker.listFocus]) handle.release();
}

export function cancelRemoteFolderPicker(view, cx) {
  if (!view.remotePicker) return;
  view.folder.focus();
  disposeRemoteFolderPicker(view);
  cx.notify();
}

export function navigateRemoteFolder(view, path, cx, mode = "list") {
  const p = view.remotePicker;
  if (!p || view.closed || p.machine !== view.machine) return;
  p.debounce?.cancel(); p.debounce = null;
  if (mode === "list") {
    p.model.query = "";
    p.location = path;
    setAddress(p, path);
  }
  p.model.request(p.machine, path, cx, { token: p.token, epoch: p.pathEpoch, more: mode === "more", search: mode === "search",
    preservePath: mode === "search" || (mode !== "list" && p.path.value() !== p.location) }, mode);
}

export function chooseRemoteFolder(view, cx) {
  const p = view.remotePicker, s = p?.model.snapshot;
  if (!s || p.debounce || p.model.loading || p.model.error || !confirmedAddress(p, s)) return;
  navigateRemoteFolder(view, s.path, cx, "choose");
}

export function renderRemoteFolderPicker(view, t) {
  const p = view.remotePicker, model = p.model, snapshot = model.snapshot;
  const dirty = !confirmedAddress(p, snapshot);
  const query = snapshot?.query ?? "";
  // The last confirmed snapshot stays mounted, even when the requested parent
  // changes. Replace it atomically on success, not with a spinner-sized list.
  const rows = snapshot?.entries ?? [];
  const count = rows.length + (snapshot?.nextCursor ? 1 : 0);
  p.at = Math.max(0, Math.min(p.at, count - 1));
  const enter = (key, cx) => {
    if (view.closed || view.remotePicker !== p) return;
    const current = model.snapshot;
    if (!current || model.loading || model.error) return;
    if (key === "more") { if (current.nextCursor && !p.debounce) navigateRemoteFolder(view, current.path, cx, "more"); return; }
    const entry = current.entries.find(e => folderChild(current.path, e.name) === key);
    if (entry && !entry.error) navigateRemoteFolder(view, folderChild(current.path, entry.name), cx);
  };
  const body = v_flex().id("remote-folder-list").relative().flex_1().min_h(80).min_w(0).bg(t.background).rounded(6)
    .border(1).border_color(t.input).track_focus(p.listFocus).role("list_box").accessibility_label("Поддиректории")
    .on_key_down((e, cx) => {
      if (view.closed || view.remotePicker !== p) return;
      if (e.key === "up" && e.modifiers?.alt) return;
      if (e.key_char && !e.modifiers?.platform && !e.modifiers?.control && !e.modifiers?.alt) {
        p.path.focus(); p.path.set_value(`${p.location.replace(/\/$/, "")}/${e.key_char}`); cx.stop_propagation(); return;
      }
      if (model.loading || model.error) return;
      if (e.key === "up" || e.key === "down") {
        p.at = Math.max(0, Math.min(count - 1, p.at + (e.key === "down" ? 1 : -1)));
        p.scroll.scroll_to_item(p.at); cx.notify(); cx.stop_propagation();
      } else if ((e.key === "enter" || e.key === "right") && count) {
        enter(p.at === rows.length ? "more" : folderChild(snapshot.path, rows[p.at].name), cx); cx.stop_propagation();
      }
    });
  const content = v_flex().id("remote-folder-content").relative().flex_1().min_h(0).w_full()
    .opacity(model.loading || model.error ? 0.3 : 1).transition("opacity", 140);
  if (!count) content.items_center().justify_center()
    .child(div().text_color(t.muted).child(!snapshot ? "" : query ? "Совпадающих директорий нет" : "Поддиректорий нет — можно выбрать эту директорию"));
  else content.child(v_virtual_list("remote-folders", count, 30,
    i => i === rows.length ? "more" : folderChild(snapshot.path, rows[i].name),
    range => Array.from({ length: range.end - range.start }, (_, offset) => {
      const i = range.start + offset, entry = rows[i];
      if (!entry) return h_flex().h(30).px(10).items_center().text_color(t.accent).child("Ещё директории…");
      return h_flex().id(`remote-folder-${entry.name}`).h(30).px(10).gap(8).items_center().min_w(0).role("list_box_option")
        .aria_selected(i === p.at).when(i === p.at, el => el.aria_active_descendant().bg(t.selected))
        .disabled(Boolean(entry.error) || model.loading).cursor_pointer().hover(el => el.bg(t.hover)).text_color(entry.error ? t.dim : t.foreground)
        .tooltip(entry.error ? directoryMessage(entry.error) : entry.link ? `Ссылка → ${entry.target ?? "директория"}` : entry.name)
        .child(svg("mods/workspace-chrome/folder.svg").size(14).flex_none().text_color(t.muted))
        .child(div().flex_1().min_w(0).truncate().child(entry.name.replace(/[\r\n]/g, " ")))
        .when(entry.link, el => el.child(div().flex_none().text_color(t.muted).child("↗")));
    })).h_full().w_full().track_scroll(p.scroll).on_item_click((key, cx) => enter(key, cx)))
    .child(Scrollbar.vertical("remote-folders").absolute().inset_0());
  body.child(content)
    .child(v_flex().id("remote-folder-loading").absolute().inset_0().items_center().justify_center()
      .opacity(model.loading ? 1 : 0).transition("opacity", 140)
      .when(!model.loading, el => el.invisible())
      .when(model.loading, el => el.on_mouse_down("left", (_e, cx) => cx.stop_propagation()).on_click((_e, cx) => cx.stop_propagation()))
      .child(h_flex().gap(8).p(12).rounded(6).bg(t.popover).items_center().role("status").accessibility_label("Читаем директорию")
        .child(svg("mods/workspace-chrome/spinner.svg").size(14).text_color(t.muted).spin(900))
        .child(div().text_color(t.foreground).child("Читаем директорию…"))))
    .when(Boolean(model.error), el => el.child(v_flex().id("remote-folder-error").absolute().inset_0().p(12).gap(10).justify_center().bg(t.background)
      .child(div().text_color(t.blocked).child(model.error))
      .child(pickerButton("remote-folder-retry", "Повторить", cx => navigateRemoteFolder(view, p.location, cx, "search"), t))));
  const viewport = globalThis.window?.viewport_size?.()?.height;
  const height = Number.isFinite(viewport) ? Math.max(220, Math.min(512, viewport - 48)) : 512;
  const status = p.debounce ? "Ожидаем завершения ввода…" : model.loading ? "Загрузка…" : model.error ? "Директория не подтверждена"
    : snapshot?.limited ? "Часть списка скрыта — уточните путь вручную" : query ? `${rows.length} совпадений · ${snapshot?.path}`
    : snapshot?.realPath !== snapshot?.path ? `Ссылка → ${snapshot?.realPath}` : `${rows.length} поддиректорий`;
  return v_flex().id("remote-folder-picker").track_focus(p.focus).w_full().h(height).min_h(0).p(18).gap(12).bg(t.popover).text_color(t.foreground).text_size(12)
    .overflow_y_scrollbar().on_key_down((e, cx) => {
      if (view.closed || view.remotePicker !== p) return;
      if (e.key === "escape") { cancelRemoteFolderPicker(view, cx); cx.stop_propagation(); }
      else if (e.key?.toLowerCase() === "l" && (e.modifiers?.platform || e.modifiers?.control)) { p.path.focus(); cx.stop_propagation(); }
      else if (e.key === "up" && e.modifiers?.alt && snapshot) { navigateRemoteFolder(view, folderParent(snapshot.path), cx); cx.stop_propagation(); }
    })
    .child(h_flex().h(22).flex_none().gap(12).items_center().min_w(0)
      .child(div().font_semibold().text_size(15).child("Выбор директории"))
      .child(div().flex_1().min_w(0).truncate().text_right().text_size(11).text_color(t.muted).child(p.machine)))
    .child(h_flex().h(34).flex_none().gap(6).min_w(0).items_center()
      .child(iconButton("remote-folder-home", "home", "Домой", cx => navigateRemoteFolder(view, "~", cx), t))
      .child(iconButton("remote-folder-up", "arrow-up", "Вверх", cx => navigateRemoteFolder(view, folderParent(snapshot.path), cx), t, Boolean(snapshot && snapshot.path !== "/")))
      .child(iconButton("remote-folder-hidden", model.hidden ? "eye" : "eye-off", model.hidden ? "Скрыть скрытые директории" : "Показать скрытые директории", cx => { model.hidden = !model.hidden; navigateRemoteFolder(view, p.location, cx, "search"); }, t)
        .selected(model.hidden).when(model.hidden, el => el.bg(t.selected).text_color(t.accent)))
      .child(Input.new(p.path).id("remote-folder-path").flex_1().min_w(0).h(34).px(9).rounded(6).border(1).border_color(t.input).bg(t.background)
        .accessibility_label("Текущая директория на удалённой машине"))
      .child(pickerButton("remote-folder-go", "Перейти", cx => navigateRemoteFolder(view, p.path.value(), cx), t)))
    .child(div().h(16).flex_none().truncate().text_size(10.5).text_color(t.muted).tooltip(status).child(status))
    .child(body)
    .child(h_flex().h(34).flex_none().gap(8).justify_end()
      .child(pickerButton("remote-folder-cancel", "Отмена", cx => cancelRemoteFolderPicker(view, cx), t))
      .child(pickerButton("remote-folder-choose", "Выбрать директорию", cx => chooseRemoteFolder(view, cx), t, Boolean(snapshot && !p.debounce && !model.loading && !model.error && !dirty), true)));
}
