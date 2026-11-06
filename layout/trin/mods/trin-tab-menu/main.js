// Tab context actions retain native tab IDs; new work always uses the project root.
import { View, div } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { props, run, theme, closeOverlay, openOverlay } from "herdr";

export default class TabMenu extends View {
  init(_props, cx) { this.handle = cx.focus_handle(); this.handle.focus(); this.at = 0; }
  items() {
    const target = props()?.target ?? {};
    return [
      ["Переименовать", () => run("tab.rename", { id: target.id })],
      ["Новая вкладка", () => run(`trin-projects.new-tab${target.project ? "--" + encodeURIComponent(target.project) : ""}`)],
      ["Запустить агента…", () => openOverlay({ mod: "trin-projects", kind: "dialog", width: 520, height: 390, props: { surface: "agents", project: target.project } })],
      ["Закрыть вкладку", () => run("tab.close", { id: target.id })]
    ];
  }
  choose(i) { const item = this.items()[i]; if (item) { closeOverlay(props().overlay); item[1](); } }
  render() {
    const t = theme(), list = this.items();
    return v_flex().id("project-tab-menu").track_focus(this.handle).w_full().min_h(0).p(5).bg(t.popover).text_size(12.8)
      .on_key_down((e, cx) => {
        if (e.key === "escape") closeOverlay(props().overlay);
        else if (e.key === "enter") this.choose(this.at);
        else if (e.key === "down") this.at = (this.at + 1) % list.length;
        else if (e.key === "up") this.at = (this.at + list.length - 1) % list.length;
        else return;
        cx.stop_propagation(); cx.notify();
      })
      .children(list.map(([text], i) => h_flex().id(`project-tab-action-${i}`).h(31).px(9).items_center().rounded(6).text_color(t.foreground)
        .when(i === this.at, el => el.bg(t.selected)).hover(el => el.bg(t.hover)).on_click(() => this.choose(i)).child(text)));
  }
}
