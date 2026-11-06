// Opt-in eager native check of the form, not merely the sidebar's initial render.
import { View } from "gpui-kit";
import { initProjectForm, renderProjectForm } from "./form.js";
export default class FormCheck extends View {
  init(_props, cx) {
    this.handle = cx.focus_handle();
    this.error = "";
    this.closed = false;
    initProjectForm(this, cx);
  }
  render() { return renderProjectForm(this); }
}
