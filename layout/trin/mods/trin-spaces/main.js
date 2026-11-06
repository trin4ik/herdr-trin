// Workspace spaces: stable colored avatars, worktrees under their checkout,
// grouped by machine when more than one is known. After a space's name, the repo
// its first terminal is in now, only where unusual: repo and branch. Agent
// badges summarize the space. Click to switch, right-click for its menu,
// drag to reorder within a machine.

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { spaces, agents, machines, panes, run, openOverlay, exec, hc, theme, shared, setShared, watchFolder, window, layout } from "herdr";

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

// Where a space is now: its first terminal's live folder, else where it was made.
// `ps`: panes(), read once by the caller (each read copies every pane).
function place(s, ps) {
  const tabs = new Set((s.tabs ?? []).map((c) => c.id));
  return ps.find((p) => tabs.has(p.tab) && p.cwd)?.cwd || s.folder;
}
const rowHeight = ({ tree }) => (tree ? 28 : 30);
const where = (s, ps) => JSON.stringify([s.machine ?? null, place(s, ps)]);
const base = (path) => path.replace(/\/+$/, "").split("/").pop();
// Safety reconciliation for remote Git, failed watchers and machine details
// not exposed by Herdr's reactive machine view. Not the primary update path.
const FALLBACK = 60000;

// What a stored color name looks like on an avatar (the same in every mod
// that draws space avatars): one strong color per hue, light and dark alike.
const MARK = { ansi6: "#22a99a", ansi3: "#e3814f", ansi4: "#5b8def", ansi5: "#8b7cf6", ansi2: "#3fae6a", ansi1: "#d9649a" };

function agentSummary(list) {
  const rank = { blocked: 4, done: 3, working: 2, idle: 1 };
  const result = new Map();
  for (const a of list) {
    const info = result.get(a.space) ?? { count: 0, working: false, status: null };
    info.count++;
    info.working ||= a.status === "working";
    if ((rank[a.status] ?? 0) > (rank[info.status] ?? 0)) info.status = a.status;
    result.set(a.space, info);
  }
  return result;
}

export default class Spaces extends View {
  init(_props, cx) {
    this.closed = false;
    this.git = {}; // where() -> { name, branch, repo, linked }, null: not a repo
    this.checked = {}; // where() -> when it was last looked at
    this.unsure = new Set(); // where()s git couldn't answer for (a machine away)
    this.pending = new Set(); // where()s to look at next
    this.checking = new Set(); // reads already running, not requeued by render
    this.busy = {}; // pane -> an agent working or a program in front of its shell
    this.ready = {}; // machine -> its layout was readable at the last look
    this.at = {}; // space -> its where() at the last look
    this.watching = new Set(); // local repos' git folders
    // Spaces made while this runs wait (at most a moment) for their git, so a
    // worktree appears under its repo instead of jumping there.
    this.shown = new Set(spaces().map((s) => s.id));
    this.born = {};
    this.pressed = null;
    this.over = null;
    this.machines = [];
    this.machineSignature = null;
    this.machineRefreshBusy = false;
    this.machineRefreshPending = false;
    this.folded = new Set(JSON.parse(localStorage.getItem("machines.folded") ?? "[]"));
    this.syncMachines(cx);
    this.checkNew(cx, false);
    // Herdr render invalidations and local filesystem watches drive updates.
    // One slow fallback remains where the installed API cannot push details.
    cx.timer.every(FALLBACK, cx => { this.checkNew(cx); this.refreshMachines(cx); });
  }

  close() {
    this.closed = true;
    this.watching.clear();
    this.pending.clear();
  }

  syncMachines(cx) {
    const signature = JSON.stringify(machines());
    if (signature === this.machineSignature) return;
    this.machineSignature = signature;
    this.refreshMachines(cx);
  }

  checkNew(cx, notify = true) {
    if (this.closed) return;
    const now = Date.now();
    const sp = spaces();
    for (const s of sp) if (!this.shown.has(s.id) && this.born[s.id] == null) {
      this.born[s.id] = now;
      // One-shot grace for a new space's Git identity, not a polling loop.
      cx.timer.after(1500, cx => { if (!this.closed && !this.shown.has(s.id)) cx.notify(); });
    }
    const due = new Set([...this.due(now, sp, panes())].filter(k => !this.checking.has(k) && !this.pending.has(k)));
    if (due.size) this.refreshGit(due, cx);
    if (notify && sp.some(s => !this.shown.has(s.id) && now - this.born[s.id] >= 1500)) cx.notify();
  }

  // Places whose git could have changed: new ones (a new space, a cd), where a
  // command or an agent's turn just ended (a checkout, a commit, edits), those on
  // a machine that is back, and repos in view whose fallback is due.
  due(now, sp, ps) {
    // Moved there: what it knew of that place may be old.
    const at = {};
    for (const s of sp) at[s.id] = where(s, ps);
    const keys = new Set(Object.values(at).filter((k) => JSON.parse(k)[1]));
    const due = new Set([...keys].filter((k) => !(k in this.git)));
    for (const [id, k] of Object.entries(at)) if (this.at[id] && this.at[id] !== k && keys.has(k)) due.add(k);
    this.at = at;
    const spaceOf = {};
    for (const s of sp) for (const t of s.tabs ?? []) spaceOf[t.id] = s;
    const busy = {};
    for (const p of ps) {
      if (!p.terminal || !spaceOf[p.tab]) continue;
      busy[p.pane] = p.status === "working" || !(p.program?.shell || p.program?.agent);
      if (this.busy[p.pane] && !busy[p.pane]) this.sameRepo(at[spaceOf[p.tab].id], keys).forEach((k) => due.add(k));
    }
    this.busy = busy;
    for (const m of machines()) {
      const ready = m.layout === "ready";
      if (ready && this.ready[m.name] === false) for (const k of keys) if (JSON.parse(k)[0] === m.name) due.add(k);
      this.ready[m.name] = ready;
    }
    if (window()?.active) {
      for (const k of keys) {
        const asks = this.git[k] || this.unsure.has(k);
        if (asks && !this.folded.has(JSON.parse(k)[0]) && now - (this.checked[k] ?? 0) >= FALLBACK) due.add(k);
      }
    }
    return due;
  }

  // `key` and the other places in its repo (a checkout can change their branches).
  sameRepo(key, keys) {
    const repo = this.git[key]?.repo;
    return [key, ...(repo ? [...keys].filter((k) => this.git[k]?.repo === repo) : [])];
  }

  refreshMachines(cx) {
    if (this.closed) return;
    if (this.machineRefreshBusy) { this.machineRefreshPending = true; return; }
    this.machineRefreshBusy = true;
    cx.spawn(async cx => {
      try {
        const r = await hc(["machine", "ls", "--json"]);
        if (!this.closed && r.code === 0) {
          const next = (JSON.parse(r.stdout).machines ?? []).filter(m => !m.temporary);
          if (JSON.stringify(next) !== JSON.stringify(this.machines)) { this.machines = next; cx.notify(); }
        }
      } catch {} finally {
        this.machineRefreshBusy = false;
        if (this.machineRefreshPending) {
          this.machineRefreshPending = false;
          this.refreshMachines(cx);
        }
      }
    });
  }

  // One look at a time, each place once however often it was asked for meanwhile.
  refreshGit(keys, cx) {
    if (this.closed) return;
    for (const k of keys) this.pending.add(k);
    if (this.refreshing) return;
    this.refreshing = true;
    cx.spawn(async (cx) => {
      while (!this.closed && this.pending.size) {
        const keys = [...this.pending];
        this.pending.clear();
        for (const key of keys) {
          if (this.closed) break;
          this.checking.add(key);
          try { await this.check(key, cx); } finally { this.checking.delete(key); }
        }
      }
      const used = new Set(Object.values(this.git).filter(Boolean).map((g) => JSON.parse(g.repo)).filter(([on]) => !on).map(([, repo]) => repo));
      for (const repo of this.watching) if (!used.has(repo)) this.watching.delete(repo);
      this.refreshing = false;
      if (!this.closed) cx.notify();
    });
  }

  // One place's git: branch and repository/worktree identity.
  async check(key, cx) {
    const [on, cwd] = JSON.parse(key);
    this.checked[key] = Date.now();
    this.unsure.delete(key);
    try {
      // No optional locks: a status that touched the index would wake the watch again.
      // Writes nothing under .git (index lock, fsmonitor cookie): watch() would wake on it.
      const r = await exec("git", ["--no-optional-locks", "-c", "core.fsmonitor=false", "status", "--porcelain=v2", "--branch"], { cwd, on });
      if (this.closed) return;
      if (r.code !== 0) {
        // Only git's own "no" is remembered as no repo; anything else is asked again.
        if (!/not a git repository/i.test(r.stderr)) this.unsure.add(key);
        this.git[key] = null;
        return;
      }
      // The repo's own .git folder names it; a worktree's top differs from it.
      const top = await exec("git", ["rev-parse", "--show-toplevel"], { cwd, on });
      const common = await exec("git", ["rev-parse", "--path-format=absolute", "--git-common-dir"], { cwd, on });
      if (this.closed) return;
      const repo = common.stdout.trim();
      const name = repo.endsWith("/.git") ? base(repo.slice(0, -5)) : base(repo).replace(/\.git$/, "");
      const info = { name, branch: null, repo: JSON.stringify([on, repo]), linked: repo !== `${top.stdout.trim()}/.git` };
      for (const line of r.stdout.split("\n")) {
        if (line.startsWith("# branch.head ")) info.branch = line.slice(14);
      }
      this.git[key] = info;
      if (!on) this.watch(repo, cx);
    } catch {
      // Couldn't ask (the machine is away): keep what it showed, look again later.
      this.git[key] ??= null;
      this.unsure.add(key);
    }
  }

  watch(repo, cx) {
    if (this.closed || this.watching.has(repo)) return;
    this.watching.add(repo);
    cx.spawn(async (cx) => {
      try {
        let initial = true;
        while (this.watching.has(repo)) {
          const mine = JSON.stringify([null, repo]);
          const changed = watchFolder(repo);
          // Register before reconciliation so changes during discovery are
          // covered by either the fresh read or the pending filesystem event.
          if (initial) {
            initial = false;
            this.refreshGit(Object.keys(this.git).filter(k => this.git[k]?.repo === mine), cx);
          }
          await changed;
          if (this.watching.has(repo)) this.refreshGit(Object.keys(this.git).filter((k) => this.git[k]?.repo === mine), cx);
        }
      } catch { this.watching.delete(repo); }
    });
  }

  menu(w) {
    // A popup is as tall as its items; height is only the most.
    openOverlay({ mod: "menu", kind: "popup", width: 200, height: 260, props: { tab: "space", target: { id: w.id } } });
  }

  // Spaces in order, each worktree space moved under its main checkout's.
  ordered() {
    const sp = spaces(), ps = panes();
    const ready = (w) => this.shown.has(w.id) || where(w, ps) in this.git || Date.now() - (this.born[w.id] ??= Date.now()) >= 1500;
    for (const w of sp) if (!this.shown.has(w.id) && ready(w)) this.shown.add(w.id);
    const list = sp.map((w, i) => ({ w, i, g: this.git[where(w, ps)] ?? null })).filter((r) => this.shown.has(r.w.id));
    const mains = new Set(list.filter((r) => r.g && !r.g.linked).map((r) => r.g.repo));
    const nested = (r) => r.g?.linked && mains.has(r.g.repo);
    const out = [];
    const placed = new Set(); // repos whose worktrees are listed (under the first of its spaces)
    for (const r of list) {
      if (nested(r)) continue;
      out.push({ ...r, tree: null });
      if (!r.g || r.g.linked || placed.has(r.g.repo)) continue;
      placed.add(r.g.repo);
      const kids = list.filter((k) => nested(k) && k.g.repo === r.g.repo);
      kids.forEach((k, n) => out.push({ ...k, tree: n === kids.length - 1 ? "└─" : "├─" }));
    }
    return out;
  }

  // The rows, under a header per machine once one isn't this computer.
  grouped(t, collapsed) {
    const rows = this.ordered();
    // The rail is a project switcher, not a squeezed machine tree. Show all
    // projects even when a machine section was folded in the expanded view.
    if (collapsed) return rows.map(r => this.compact(r, t));
    const draw = (r) => (collapsed ? this.compact(r, t) : this.row(r, t));
    const remote = [...new Set([...rows.map(r => r.w.machine).filter(Boolean), ...this.machines.map(m => m.name)])].sort();
    const loading = m => {
      const layout = machines().find(k => k.name === m)?.layout;
      return layout === "loading" || (!layout && this.machines.some(k => k.name === m && k.state === "connected"))
        || spaces().some(s => (s.machine ?? null) === m && !this.shown.has(s.id));
    };
    const loadingRow = m => div().id(`machine-loading-${m ?? "local"}`).h(28).flex_none().px(8).text_size(12).text_color(t.dim).child(collapsed ? "…" : "Loading…");
    if (!remote.length) return loading(null) && !rows.length ? [loadingRow(null)] : rows.map(draw);
    const machineNames = [null, ...remote];
    return machineNames.flatMap((m) => {
      const mine = rows.filter((r) => (r.w.machine ?? null) === m);
      const machine = this.machines.find(k => k.name === m);
      const connected = !m || machine?.state === "connected";
      const folded = this.folded.has(m);
      const head = h_flex().id(`machine-${m ?? "local"}`).h(26).flex_none().px(collapsed ? 2 : 8).gap(7).items_center().text_size(11).text_color(t.dim)
        .cursor_pointer().hover(s => s.bg(t.hover))
        .on_click((_e, cx) => {
          folded ? this.folded.delete(m) : this.folded.add(m);
          localStorage.setItem("machines.folded", JSON.stringify([...this.folded]));
          cx.notify();
        })
        .child(svg(`mods/workspace-chrome/${m ? "server" : "monitor"}.svg`).size(13).flex_none())
        .when(!collapsed, el => el.child(div().flex_1().truncate().child(m ?? "This computer"))
          .when(!connected, el => el.child(div().flex_none().child((machine?.state ?? "asleep").replace(/_/g, " "))))
          .when(connected, el => el.child(div().id(`machine-add-${m ?? "local"}`).flex_none().px(4).rounded(4).hover(s => s.bg(t.hover).text_color(t.foreground))
            .on_click((_e, cx) => { cx.stop_propagation(); run("space.new", m ? { machine: m } : {}); }).child("+")))
          .child(div().size(6).rounded(3).flex_none().bg(connected ? t.ansi2 : t.dim))
          .child(svg(`mods/workspace-chrome/chevron-${folded ? "right" : "down"}.svg`).size(11).flex_none()));
      const empty = h_flex().id(`machine-new-${m ?? "local"}`).h(28).flex_none().px(8).gap(7).items_center().rounded(7)
        .text_size(12).text_color(t.dim).hover(s => s.bg(t.hover).text_color(t.foreground))
        .on_click(() => run("space.new", m ? { machine: m } : {}))
        .child(svg("mods/workspace-chrome/plus.svg").size(12)).when(!collapsed, el => el.child("New space there"));
      return [head, ...(folded ? [] : mine.length ? mine.map(draw) : [loading(m) ? loadingRow(m) : empty])];
    });
  }

  // Only what's unusual, after the name: "herdr \u00b7 spike". The repo or
  // branch goes when the name already says it (a worktree is named after its branch),
  // and master/main go: nearly every space is on one.
  gitTail(g, tree, name, t) {
    const raw = g.branch ?? "detached";
    const same = (a) => a.toLowerCase() === name.toLowerCase();
    const branch = same(raw) || raw === "master" || raw === "main" ? "" : raw;
    const repo = tree || same(g.name) ? "" : g.name;
    const where = [repo, branch].filter(Boolean).join(" \u00b7 ");
    return div()
      .flex_1()
      .min_w(0)
      .truncate()
      .text_right()
      .text_size(11.5)
      .text_color(t.dim)
      .child(where);
  }

  agentBadge(w, t) {
    const info = this.spaceAgents.get(w.id) ?? { count: 0, working: false, status: null };
    const color = t[info.status] ?? t.muted;
    const active = info.status && info.status !== "idle";
    return h_flex().id(`space-agents-${w.id}`).flex_none().gap(5).items_center()
      .tooltip(`${info.count} agents in this space · ${info.status ?? "no agents"}${info.working && info.status !== "working" ? " · working" : ""}`)
      .when(info.working, el => el.child(svg("mods/workspace-chrome/spinner.svg")
        .size(12).flex_none().text_color(t.working).spin(900)))
      .child(div().flex_none().min_w(18).h(18).px(5).rounded(5).bg(active ? color + "26" : t.raised)
        .text_size(10.5).font_medium().text_color(color)
        .flex().items_center().justify_center().child(String(info.count)));
  }

  row({ w, i, g, tree }, t) {
    const on = w.active;
    const colors = shared("space.colors") ?? {};
    const color = colors[w.id] ?? "ansi6";
    const fill = color.startsWith("#") ? color : (MARK[color] ?? t[color]);
    const sourceIndex = spaces().findIndex(s => s.id === this.pressed);
    return v_flex()
      .relative()
      .id(`space-${w.id}`)
      .px(8)
      .h(rowHeight({ tree }))
      .flex_none()
      .justify_center()
      .rounded(8)
      .when(on, (el) => el.bg(t.selected))
      .when(!on, (el) => el.hover((s) => s.bg(t.hover)))
      .when(this.pressed === w.id && this.over !== w.id, (el) => el.opacity(0.5))
      .when(sourceIndex >= 0 && this.over === w.id && this.pressed !== w.id, (el) => el.child(
        div().absolute().left(0).right(0).h(2).bg(t.accent)
          .when(sourceIndex < i, line => line.bottom(0))
          .when(sourceIndex > i, line => line.top(0)),
      ))
      .on_mouse_down("left", () => {
        this.pressed = w.id;
        this.over = w.id;
      })
      .on_mouse_move((_e, cx) => {
        const source = spaces().find(s => s.id === this.pressed);
        const over = source && (source.machine ?? null) === (w.machine ?? null) ? w.id : null;
        if (this.pressed !== null && this.over !== over) {
          this.over = over;
          cx.notify();
        }
      })
      .on_mouse_up("left", (_e, cx) => {
        const from = this.pressed;
        const over = this.over;
        this.pressed = null;
        this.over = null;
        const current = spaces();
        const source = current.find(s => s.id === from);
        const target = current.findIndex(s => s.id === w.id);
        if (source && target >= 0 && (source.machine ?? null) === (current[target].machine ?? null)) {
          if (over === w.id && from !== w.id) run("space.move", { id: from, index: target + 1 });
          else if (from === w.id) run("space.select", { id: w.id });
        }
        cx.notify();
      })
      .on_mouse_down("right", () => this.menu(w))
      .child(
        h_flex()
          .gap(9)
          .items_center()
          .when(Boolean(tree), el => el.pl(22).child(svg("mods/workspace-chrome/branch.svg").size(13).text_color(t.dim)))
          .when(!tree, el => el.child(h_flex().size(18).flex_none().rounded(5).bg(fill).items_center().justify_center().text_size(10.5).font_bold().text_color("#ffffff").child([...w.name][0]?.toUpperCase() ?? "?")))
          .child(div().min_w(0).max_w(150).truncate().text_color(t.foreground).child(w.name))
          .child(g ? this.gitTail(g, tree, w.name, t) : div().flex_1())
          .child(this.agentBadge(w, t)),
      );
  }

  compact({ w }, t) {
    const color = (shared("space.colors") ?? {})[w.id] ?? "ansi6";
    const fill = color.startsWith("#") ? color : (MARK[color] ?? t[color]);
    return h_flex()
      .id(`space-${w.id}`)
      .h(36)
      .flex_none()
      .justify_center().items_center().cursor_pointer()
      .tooltip(`${w.name}\n${w.machine || "This computer"}${w.folder ? `\n${w.folder}` : ""}`)
      .on_click(() => run("space.select", { id: w.id }))
      .on_mouse_down("right", () => this.menu(w))
      .child(h_flex().size(32).flex_none().rounded(7).items_center().justify_center()
        .when(w.active, el => el.bg(t.selected).border(1).border_color(t.accent))
        .when(!w.active, el => el.hover(s => s.bg(t.hover)))
        .child(h_flex().size(22).flex_none().rounded(5).bg(fill).items_center().justify_center()
          .text_size(12).font_bold().text_color("#ffffff").child([...w.name][0]?.toUpperCase() ?? "?")));
  }

  render(cx) {
    if (cx) { this.syncMachines(cx); this.checkNew(cx, false); }
    const t = theme();
    const collapsed = narrow();
    const palette = ["ansi6", "ansi3", "ansi4", "ansi5", "ansi2", "ansi1"];
    const sp = spaces();
    const ids = new Set(sp.map(p => p.id));
    this.spaceAgents = agentSummary(agents());
    const kept = Object.fromEntries(Object.entries(shared("space.colors") ?? {}).filter(([id]) => ids.has(id)));
    let changed = Object.keys(kept).length !== Object.keys(shared("space.colors") ?? {}).length;
    for (const p of sp) {
      if (kept[p.id] || p.name === "~") continue;
      const uses = c => Object.values(kept).filter(v => v === c).length;
      kept[p.id] = palette.reduce((best, c) => uses(c) < uses(best) ? c : best);
      changed = true;
    }
    if (changed) setShared("space.colors", kept);
    return v_flex().size_full().min_h(0).bg(t.chrome).text_size(13)
      .child(h_flex().h(31).pl(collapsed ? 0 : 16).pr(collapsed ? 0 : 10).items_center()
        .when(collapsed, el => el.justify_center()).when(!collapsed, el => el.justify_between()).text_size(11.5).text_color(t.dim)
        .when(!collapsed, el => el.child("Spaces"))
        .child(div().id("space-new").size(26).flex_none().flex().items_center().justify_center().rounded(6).hover(s => s.bg(t.hover).text_color(t.foreground))
          .tooltip("Новое пространство").on_click(() => run("space.new")).child(svg("mods/workspace-chrome/plus.svg").size(14))))
      .child(v_flex().flex_1().min_h(0).px(collapsed ? 0 : 8).pb(14).overflow_y_scrollbar()
        .when(!collapsed, el => el.mode("always"))
        .on_mouse_up("left", (_e, cx) => { this.pressed = null; this.over = null; cx.notify(); })
        .children(this.grouped(t, collapsed)));
  }
}
// Modified derivative of Herdr spaces (Apache-2.0), retained for regression/rollback.
// Original modifications: MIT. See repository THIRD_PARTY.md.
