// Persistent project identity is never derived from a terminal's live cwd.
export const KEY = "trin.projects.v1";
export const PALETTE = ["ansi6", "ansi3", "ansi4", "ansi5", "ansi2", "ansi1"];
export const MARK = { ansi6: "#22a99a", ansi3: "#e3814f", ansi4: "#5b8def", ansi5: "#8b7cf6", ansi2: "#3fae6a", ansi1: "#d9649a" };

export function stateOf(value) {
  if (value == null) return { version: 1, projects: [], selected: null, ignored: [] };
  if (value.version !== 1 || !Array.isArray(value.projects) || !Array.isArray(value.ignored)) throw new Error("Неизвестный формат Projects. Реестр не изменён.");
  const ids = new Set();
  for (const p of value.projects) {
    if (!p || typeof p.id !== "string" || ids.has(p.id) || typeof p.name !== "string" || typeof p.folder !== "string"
      || (p.machine !== null && typeof p.machine !== "string") || (p.space !== null && typeof p.space !== "string")) throw new Error("Повреждён реестр Projects. Данные не перезаписаны.");
    if (p.pending && (!Number.isFinite(p.pending.at) || typeof p.pending.name !== "string" || typeof p.pending.folder !== "string"
      || !Array.isArray(p.pending.before) || p.pending.before.some(id => typeof id !== "string"))) throw new Error("Повреждена запись запуска Projects. Данные не перезаписаны.");
    ids.add(p.id);
  }
  return value;
}

export const liveSpace = (p, sp) => p && sp.find(s => s.id === p.space && (s.machine ?? null) === p.machine);
export const machineReady = (p, ms) => ms.some(m => (m.name ?? null) === p.machine && m.layout === "ready");
export const projectForSpace = (state, id) => state.projects.find(p => p.space === id);
export const selectedProject = state => state.projects.find(p => p.id === state.selected);
// A bounded launch lock is distinct from the durable uncertain-result record.
export const launchLocked = (p, now = Date.now()) => Boolean(p?.pending && !p.pending.failed && now - p.pending.at <= 35000);

export function nextColor(projects) {
  const count = color => projects.filter(p => p.color === color).length;
  return PALETTE.reduce((best, color) => count(color) < count(best) ? color : best);
}

export function adopt(state, sp, seeds = {}, colors = {}, now = Date.now()) {
  // Recover an acknowledged-or-still-finishing native launch after hot reload.
  // Match only a unique new ID against the exact saved request and its baseline.
  let changed = false;
  const projects = state.projects.map(p => {
    if (!p.pending) return p;
    const request = p.pending;
    const matches = sp.filter(s => !request.before.includes(s.id) && s.name === request.name && s.folder === request.folder
      && (s.machine ?? null) === p.machine && !state.projects.some(other => other.id !== p.id && other.space === s.id));
    if (matches.length === 1) { changed = true; return { ...p, space: matches[0].id, pending: null }; }
    // No timeout-based deletion: an offline/delayed machine can publish this
    // native Space much later. Keep correlation until a match or explicit retry.
    return p;
  });
  for (const s of sp) {
    if (projects.some(p => p.space === s.id) || state.ignored.includes(s.id)) continue;
    if (projects.some(p => p.pending && !p.pending.before.includes(s.id) && p.pending.name === s.name && p.pending.folder === s.folder && p.machine === (s.machine ?? null))) continue;
    const seed = seeds[s.id] ?? {};
    projects.push({ id: `project:${s.id}`, name: seed.name ?? s.name, folder: seed.folder ?? s.folder,
      machine: s.machine ?? null, space: s.id, color: colors[s.id] ?? nextColor(projects) });
  }
  const selected = state.selected ?? projects.find(p => sp.some(s => s.id === p.space && s.active))?.id ?? projects[0]?.id ?? null;
  return !changed && projects.length === state.projects.length && selected === state.selected ? state : { ...state, projects, selected };
}

export function updateProject(state, id, patch) {
  return { ...state, projects: state.projects.map(p => p.id === id ? { ...p, ...patch, id: p.id, machine: p.machine } : p) };
}

export function moveProject(state, source, target) {
  const a = state.projects.find(p => p.id === source), b = state.projects.find(p => p.id === target);
  if (!a || !b || a.machine !== b.machine || a === b) return state;
  const projects = state.projects.filter(p => p.id !== source);
  const to = projects.findIndex(p => p.id === target);
  const before = state.projects.indexOf(a) > state.projects.indexOf(b);
  projects.splice(to + (before ? 0 : 1), 0, a);
  return { ...state, projects };
}

export function removeProject(state, id) {
  const p = state.projects.find(p => p.id === id);
  if (!p) return state;
  const projects = state.projects.filter(p => p.id !== id);
  return { ...state, projects, selected: state.selected === id ? projects[0]?.id ?? null : state.selected,
    ignored: p.space ? [...new Set([...state.ignored, p.space])] : state.ignored };
}

export function validFolder(folder) {
  return typeof folder === "string" && !/[\x00\r\n]/.test(folder) && (/^\//.test(folder) || /^[A-Za-z]:[\\/]/.test(folder) || /^\\\\/.test(folder));
}

export function directoryName(folder) {
  const separator = folder.startsWith("/") ? /\// : /[\\/]/;
  const path = folder.startsWith("/") ? folder.replace(/\/+$/, "") : folder.replace(/[\\/]+$/, "");
  return path.split(separator).pop() || (folder.startsWith("/") ? "/" : folder);
}

export function launchArgs(p, extra = {}) {
  if (!p?.name.trim() || !validFolder(p.folder)) throw new Error("Укажите название и абсолютную директорию проекта в настройках.");
  return { ...extra, name: p.name, folder: p.folder, machine: p.machine ?? "" };
}
