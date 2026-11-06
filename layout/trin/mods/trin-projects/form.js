// Native project form. Retained inputs + event contexts, never render contexts.
// Height is the exact native row budget; scrolling only caps a small viewport.
import { div, svg } from "gpui-kit";
import { v_flex, h_flex, Button, InputState, Input, Select, Popup } from "gpui-base";
import { props, closeOverlay, theme, machines, home, pickFolder } from "herdr";
import { nextColor, validFolder, updateProject, directoryName } from "./data.js";
import { read, write, command, target } from "./store.js";
import { openRemoteFolderPicker, renderRemoteFolderPicker, cancelRemoteFolderPicker } from "./remote-picker.js";

const machineKey = machine => JSON.stringify(machine);
const machineLabel = machine => machine ?? "Этот компьютер";
const formChoices = view => [...new Set([null, ...machines().map(m => m.name).filter(Boolean), ...(view.machine ? [view.machine] : [])])];
const formButton = (id, label, action, primary = false) => Button.new(id).h(34).px(14).flex_none().rounded(6)
  .bg(primary ? theme().solid : theme().raised).text_color(primary ? theme().solid_foreground : theme().foreground)
  .text_size(12.5).font_medium().cursor_pointer().hover(el => el.opacity(0.88))
  .on_click((_e, cx) => action(cx)).child(label);

export function initProjectForm(view, cx) {
  const p = target();
  view.machine = p ? p.machine : props()?.machine ?? null;
  view.editing = Boolean(p);
  view.name = InputState.new({ value: p?.name ?? "", placeholder: "Например, мой проект" });
  view.folder = InputState.new({ value: p?.folder ?? "", placeholder: "Полный путь к директории проекта" });
  view.folderDrafts = { [machineKey(view.machine)]: view.folder.value() };
  view.fieldErrors = {};
  view.machineOpen = false;
  view.machineAt = Math.max(0, formChoices(view).indexOf(view.machine));
  view.machineFocus = cx.focus_handle();
  view.machineListFocus = cx.focus_handle();
  view.pickVersion = 0;
  view.picking = false;
  view.name.on("submit", () => view.folder.focus());
  view.folder.on("submit", (_e, cx) => saveProjectForm(view, cx));
  for (const key of ["name", "folder"]) view[key].on("change", (_e, cx) => {
    if (view.fieldErrors[key] || view.error) { delete view.fieldErrors[key]; view.error = ""; cx.notify(); }
  });
  view.name.focus();
  if (props()?.browse && view.machine !== null) openRemoteFolderPicker(view, cx);
}

export function chooseProjectMachine(view, machine, cx) {
  if (view.editing) return;
  if (view.machine !== machine) {
    if (view.remotePicker) cancelRemoteFolderPicker(view, cx);
    view.folderDrafts[machineKey(view.machine)] = view.folder.value();
    view.machine = machine;
    view.folder.set_value(view.folderDrafts[machineKey(machine)] ?? "");
    view.fieldErrors = {};
    view.error = "";
    view.pickVersion++;
    view.picking = false;
  }
  view.machineOpen = false;
  view.machineFocus.focus();
  cx.notify();
}

export function pickProjectFolder(view, cx) {
  if (view.machine !== null) { openRemoteFolderPicker(view, cx); return; }
  if (view.picking) return;
  const version = ++view.pickVersion, before = view.folder.value();
  view.picking = true;
  cx.notify();
  cx.spawn(async cx => {
    try {
      const folder = await pickFolder({ prompt: "Выбрать корень проекта" });
      if (view.closed || version !== view.pickVersion || view.machine !== null) return;
      if (folder && view.folder.value() === before) {
        view.folder.set_value(folder);
        if (!view.name.value().trim()) view.name.set_value(directoryName(folder));
        view.folderDrafts[machineKey(null)] = folder;
        delete view.fieldErrors.folder;
        view.error = "";
        view.folder.focus();
      }
    } catch {
      if (!view.closed && version === view.pickVersion) view.error = "Не удалось открыть выбор директории. Введите путь вручную.";
    } finally {
      if (!view.closed && version === view.pickVersion) { view.picking = false; cx.notify(); }
    }
  });
}

export function saveProjectForm(view, cx) {
  const state = read(), p = target();
  if (props()?.project && !p) { view.error = "Проект уже удалён."; cx.notify(); return; }
  let name = view.name.value().trim();
  let folder = view.folder.value();
  // Expand only a local user's home. Never send the Mac's home to a remote host.
  if (view.machine === null && (folder === "~" || folder.startsWith("~/"))) folder = home() + folder.slice(1);
  if (!name && validFolder(folder)) { name = directoryName(folder).trim(); view.name.set_value(name); }
  view.fieldErrors = {};
  if (!name) view.fieldErrors.name = "Введите название проекта.";
  if (!validFolder(folder)) view.fieldErrors.folder = view.machine === null ? "Укажите полный путь или выберите директорию." : "Укажите полный путь на выбранной машине, например /home/user/project.";
  if (Object.keys(view.fieldErrors).length) { cx.notify(); return; }
  if (view.machine !== null && !machines().some(m => m.name === view.machine)) {
    view.error = "Эта машина больше не добавлена. Выберите другую."; cx.notify(); return;
  }
  if (state.projects.some(k => k.id !== p?.id && k.machine === view.machine && k.folder === folder)) {
    view.fieldErrors.folder = "Проект с этой директорией на этой машине уже есть."; cx.notify(); return;
  }
  const id = p?.id ?? `project:${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  const next = p ? updateProject(state, id, { name, folder }) : { ...state, projects: [...state.projects,
    { id, name, folder, machine: view.machine, space: null, color: nextColor(state.projects) }] };
  write(next);
  closeOverlay(props().overlay);
  command("select", id);
}

function projectMachineSelect(view) {
  const t = theme(), choices = formChoices(view);
  view.machineAt = Math.max(0, Math.min(view.machineAt, choices.length - 1));
  const trigger = Button.new("project-machine-trigger").track_focus(view.machineFocus).w_full().h(36).px(10).rounded(6)
    .bg(t.background).border(1).border_color(view.machineOpen ? t.accent : t.input).text_color(t.foreground)
    .child(h_flex().w_full().min_w(0).gap(8).items_center()
      .child(svg(`mods/workspace-chrome/${view.machine ? "server" : "monitor"}.svg`).size(14).flex_none().text_color(t.muted))
      .child(div().flex_1().min_w(0).truncate().child(machineLabel(view.machine)))
      .child(svg("mods/workspace-chrome/chevron-down.svg").size(12).flex_none().text_color(t.muted)));
  const choose = cx => chooseProjectMachine(view, choices[view.machineAt], cx);
  const list = v_flex().id("project-machine-options").track_focus(view.machineListFocus).role("list_box").accessibility_label("Машина проекта")
    .w(432).max_h(220).min_h(0).p(4).rounded(7).bg(t.popover).border(1).border_color(t.border).overflow_y_scrollbar()
    .on_key_down((e, cx) => {
      if (e.key === "up" || e.key === "down") {
        view.machineAt = (view.machineAt + (e.key === "down" ? 1 : -1) + choices.length) % choices.length;
        cx.stop_propagation(); cx.notify();
      }
      else if (e.key === "enter") { choose(cx); cx.stop_propagation(); }
      else if (e.key === "escape") { view.machineOpen = false; view.machineFocus.focus(); cx.stop_propagation(); cx.notify(); }
    })
    .children(choices.map((machine, i) => h_flex().id(`project-machine-option-${i}`).h(34).flex_none().px(8).gap(8).items_center().rounded(4)
      .role("list_box_option").aria_selected(machine === view.machine).set_position(i + 1, choices.length)
      .when(i === view.machineAt, el => el.aria_active_descendant().bg(t.selected))
      .text_color(t.foreground).cursor_pointer().hover(el => el.bg(t.hover))
      .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
      .on_click((_e, cx) => chooseProjectMachine(view, machine, cx))
      .child(div().flex_1().min_w(0).truncate().child(machineLabel(machine)))
      .when(machine !== null && machines().find(m => m.name === machine)?.layout !== "ready", el => el.child(div().text_size(10.5).text_color(t.muted).child("Не подключена")))
      .child(div().w(14).flex_none().when(machine === view.machine, el => el.child(svg("mods/workspace-chrome/check.svg").size(12).text_color(t.accent))))));
  return Select.new("project-machine-select").w_full().h(36).flex_none().accessibility_label("Машина проекта")
    .track_focus(view.machineFocus).content_focus_handle(view.machineListFocus).open(view.machineOpen)
    .on_open_change((open, cx) => { view.machineOpen = open; if (open) view.machineAt = Math.max(0, choices.indexOf(view.machine)); cx.notify(); })
    .on_confirm((_e, cx) => choose(cx))
    .on_dismiss((_e, cx) => { view.machineOpen = false; cx.stop_propagation(); cx.notify(); })
    .child(Popup.new("project-machine-popup", trigger).anchor("bottom_left").when(view.machineOpen, el => el.content(list)));
}

export function renderProjectForm(view) {
  const t = theme(), p = target();
  if (view.remotePicker) return renderRemoteFolderPicker(view, t);
  const viewport = globalThis.window?.viewport_size?.()?.height;
  const cap = Number.isFinite(viewport) ? Math.max(160, viewport - 48) : 600;
  // 40 padding + 22 heading + 3×(16 label + 6 gap + 36 control)
  // + 32 help + 34 footer + 5×16 section gaps. Errors add their own rows.
  const height = Math.min(cap, 382 + Object.keys(view.fieldErrors).length * 38 + (view.error ? 48 : 0));
  const label = text => div().h(16).flex_none().text_size(11.5).text_color(t.muted).child(text);
  const group = (text, content, error) => v_flex().flex_none().min_w(0).gap(6).child(label(text)).child(content)
    .when(Boolean(error), el => el.child(div().h(32).flex_none().line_height("16px").text_size(11.5).text_color(t.blocked).child(error)));
  const input = (key, description) => Input.new(view[key]).id(`project-${key}-input`).w_full().h(36).flex_none().px(10).rounded(6)
    .border(1).border_color(view.fieldErrors[key] ? t.blocked : t.input).bg(t.background).text_size(12.8).text_color(t.foreground).accessibility_label(description);
  return v_flex().id("projects-form").track_focus(view.handle).w_full().min_w(0).min_h(0).h(height).max_h(cap).p(20).gap(16)
    .bg(t.popover).text_color(t.foreground).text_size(12.8).overflow_y_scrollbar()
    .on_key_down((e, cx) => {
      // Let Select consume Escape first: closing its list must not close the form.
      if (e.key === "escape") {
        if (view.machineOpen) { view.machineOpen = false; view.machineFocus.focus(); cx.notify(); }
        else closeOverlay(props().overlay);
        cx.stop_propagation();
      }
    })
    .child(div().h(22).flex_none().text_size(16).font_semibold().child(p ? "Настройки проекта" : "Новый проект"))
    .child(group("Машина", p ? h_flex().h(36).gap(8).items_center()
      .child(svg(`mods/workspace-chrome/${p.machine ? "server" : "monitor"}.svg`).size(14).text_color(t.muted))
      .child(div().min_w(0).truncate().child(machineLabel(p.machine))) : projectMachineSelect(view)))
    .child(group("Название", input("name", "Название проекта"), view.fieldErrors.name))
    .child(group("Корневая директория", h_flex().min_w(0).gap(8).items_center()
      .child(div().flex_1().min_w(0).child(input("folder", "Корневая директория проекта")))
      .child(formButton("project-pick-folder", "Выбрать…", cx => pickProjectFolder(view, cx))
        .disabled(view.picking).tooltip(view.machine === null ? "Выбрать директорию на этом компьютере" : `Выбрать директорию на ${view.machine}`)), view.fieldErrors.folder))
    .child(div().h(32).flex_none().line_height("16px").text_size(11.5).text_color(t.muted).child(view.machine === null
      ? "Директория на этом компьютере.\nНовые сессии будут открываться здесь."
      : "Директория на выбранной машине.\nНовые сессии будут открываться здесь."))
    .when(Boolean(view.error), el => el.child(div().h(32).flex_none().line_height("16px").text_size(11.5).text_color(t.blocked).child(view.error)))
    .child(h_flex().h(34).flex_none().gap(8).justify_end()
      .child(formButton("project-form-cancel", "Отмена", () => closeOverlay(props().overlay)))
      .child(formButton("project-form-save", p ? "Сохранить" : "Создать проект", cx => saveProjectForm(view, cx), true)));
}
