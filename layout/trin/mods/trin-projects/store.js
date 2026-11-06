import { shared, setShared, spaces, machines, run, openOverlay, props } from "herdr";
import { KEY, stateOf, liveSpace, machineReady, selectedProject } from "./data.js";

export const read = () => stateOf(shared(KEY));
export function write(state) { setShared(KEY, state); }
export const current = () => selectedProject(read());
export const live = p => liveSpace(p, spaces());
export const ready = p => machineReady(p, machines());
export function command(action, id = null) {
  run(`trin-projects.${action}${id ? "--" + encodeURIComponent(id) : ""}`);
}
export function overlay(surface, extra = {}) {
  openOverlay({ mod: "trin-projects", kind: surface === "menu" ? "popup" : "dialog", width: surface === "menu" ? 250 : surface === "form" ? 472 : 520,
    ...(surface === "form" ? {} : { height: surface === "menu" ? 230 : surface === "picker" ? 480 : 390 }), props: { surface, ...extra } });
}
export const target = () => read().projects.find(p => p.id === props()?.project);
