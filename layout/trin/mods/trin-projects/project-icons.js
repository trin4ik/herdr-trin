import { Buffer } from "buffer";
import { createHash, randomBytes } from "crypto";
import { mkdir, readFile, unlink, writeFile } from "fs/promises";
import { appHome, exec, readTextFile, setShared, shared, watchFolder } from "herdr";

const STORE = "trin.project-icons.v1";
const CACHE = "data/trin-project-icons";
const RESYNC_MS = 90_000;
const MAX_WATCHES = 24;
const KEY = /^data\/trin-project-icons\/[a-f0-9]{64}\.png$/;
// Session and Projects views share the persisted index. A fresh Projects
// instance rechecks cached bytes before republishing the startup snapshot.
const LIVE = "trin.project-icons.ready";

export function iconKey(p) {
  return JSON.stringify([p?.machine ?? null, p?.folder ?? ""]);
}

export function projectIcon(p) {
  const path = shared(LIVE)?.[iconKey(p)];
  return typeof path === "string" && KEY.test(path) ? path : null;
}

export class ProjectIcons {
  constructor() {
    this.entries = new Map();
    this.active = 0;
    this.writes = new Map();
    this.closed = false;
    this.saved = shared(STORE) ?? {};
    this.published = shared(LIVE) ?? {};
  }

  sync(projects, machines, cx) {
    if (this.closed) return;
    const ready = new Set(machines.filter(m => m.layout === "ready").map(m => m.name ?? null));
    const keep = new Set();
    let watches = [...this.entries.values()].filter(entry => entry.watching).length, changed = false;
    for (const p of projects) {
      // Folder strings are only passed as literal process arguments.
      if (typeof p.folder !== "string" || !p.folder.startsWith("/") || p.folder === "/" || /[\u0000-\u001f]/.test(p.folder)) continue;
      const key = iconKey(p), machine = p.machine ?? null;
      keep.add(key);
      let entry = this.entries.get(key);
      if (!entry) {
        const seed = this.saved[key];
        entry = { key, folder: p.folder, machine, due: true, path: null,
          seed: typeof seed === "string" && KEY.test(seed) ? seed : null };
        this.entries.set(key, entry);
        if (entry.seed) this.verifySeed(entry, cx);
        changed = true;
      }
      const available = ready.has(machine);
      if (available && !entry.ready) entry.due = true;
      if (available !== entry.ready) entry.generation = (entry.generation ?? 0) + 1;
      entry.ready = available;
      if (!machine && available && entry.canWatch && !entry.watchFailed && !entry.watching && watches < MAX_WATCHES) {
        watches++;
        this.watch(entry, cx);
      }
    }
    for (const [key, entry] of this.entries) if (!keep.has(key)) {
      this.entries.delete(key);
      entry.watch?.cancel();
      changed = true;
    }
    if (!this.timer) this.timer = cx.timer.every(RESYNC_MS, cx => {
      for (const entry of this.entries.values()) {
        if (entry.machine || !entry.watch || entry.retry) entry.due = true;
      }
      this.verifyPublished(cx);
      this.pump(cx);
    });
    if (changed) this.publish();
    this.pump(cx);
  }

  watch(entry, cx) {
    entry.watching = true;
    cx.spawn(async cx => {
      const path = `${entry.folder.replace(/\/+$/, "")}/.herdr`;
      try {
        let initial = true;
        while (this.current(entry)) {
          entry.watch = watchFolder(path);
          if (initial) {
            // Reconcile after subscribing: a save between the first read
            // and the subscription won't deliver a filesystem event.
            initial = false;
            entry.due = true;
            this.pump(cx);
          }
          const paths = await entry.watch;
          entry.watch = null;
          if (!this.current(entry)) return;
          if (!paths.length) { entry.watchFailed = true; return; }
          if (paths.some(p => p === path || p === path + "/icon.png")) {
            entry.due = true;
            this.pump(cx);
          }
        }
      } catch {
        entry.watchFailed = true;
      } finally {
        entry.watch = null;
        entry.watching = false;
      }
    });
  }

  current(entry) {
    return !this.closed && this.entries.get(entry.key) === entry;
  }

  async matches(path, expected) {
    try {
      const bytes = await readFile(`${appHome()}/${path}`);
      return bytes.length <= 163840 && createHash("sha256").update(bytes).digest("hex") === expected;
    } catch { return false; }
  }

  verifySeed(entry, cx) {
    const path = entry.seed;
    cx.spawn(async cx => {
      const valid = await this.matches(path, path.slice(CACHE.length + 1, -4));
      if (!this.current(entry) || entry.seed !== path) return;
      entry.seed = null;
      if (valid) { entry.path = path; entry.canWatch = true; entry.cachedRevision = 1; }
      this.publish();
      cx.notify();
    });
  }

  verifyPublished(cx) {
    if (this.closed || this.verifying) return;
    const snapshot = [...this.entries.values()].filter(e => e.path)
      .map(entry => ({ entry, path: entry.path, revision: entry.cachedRevision }));
    if (!snapshot.length) return;
    this.verifying = true;
    cx.spawn(async cx => {
      const validPaths = new Map();
      let changed = false;
      try {
        for (const { entry, path, revision } of snapshot) {
          if (this.closed) return;
          if (!validPaths.has(path)) validPaths.set(path, await this.matches(path, path.slice(CACHE.length + 1, -4)));
          if (!validPaths.get(path) && this.current(entry) && entry.path === path && entry.cachedRevision === revision) {
            entry.path = null;
            entry.retry = true;
            if (!entry.busy) entry.due = true;
            changed = true;
          }
        }
        if (changed && !this.closed) {
          this.publish();
          cx.notify();
          this.pump(cx);
        }
      } finally { this.verifying = false; }
    });
  }

  async store(path, bytes, digest) {
    const full = `${appHome()}/${path}`;
    if (await this.matches(path, digest)) return;
    await mkdir(`${appHome()}/${CACHE}`, { recursive: true });
    const temp = `${full}.${randomBytes(8).toString("hex")}.tmp`;
    try {
      await writeFile(temp, bytes);
      // The host fs module does not expose rename. Move within the same
      // directory on the viewing Mac so the renderer never sees half a PNG.
      const result = await exec("/bin/mv", ["-f", temp, full], { on: null, timeout: 2000 });
      if (result.code !== 0) throw Error("Project icon cache write failed");
    } finally {
      await unlink(temp).catch(() => {});
    }
  }

  pump(cx) {
    if (this.closed) return;
    while (this.active < 2) {
      const pending = [...this.entries.values()].filter(e => e.due && e.ready && !e.busy);
      if (!pending.length) return;
      const { machine } = pending[0];
      const batch = pending.filter(e => e.machine === machine).slice(0, 8);
      const generations = batch.map(e => e.generation);
      for (const entry of batch) { entry.due = false; entry.busy = true; entry.retry = true; }
      this.active++;
      cx.spawn(async cx => {
        try {
          const source = await (this.reader ??= readTextFile(`${appHome()}/mods/trin-projects/project-icons.py`).catch(() => null));
          if (!source) { this.reader = null; return; }
          const result = await exec("python3", ["-B", "-c", source, JSON.stringify(batch.map(e => e.folder))], { on: machine, timeout: 5000 });
          if (result.code !== 0 || this.closed) return;
          const payload = JSON.parse(result.stdout);
          if (!Array.isArray(payload) || payload.length !== batch.length) return;
          for (const [index, entry] of batch.entries()) {
            const content = payload[index];
            const valid = () => this.current(entry) && entry.ready && entry.generation === generations[index];
            if (!valid() || typeof content !== "string" || content.length > 220_000) continue;
            if (!content) { entry.path = null; entry.seed = null; entry.retry = false; continue; }
            try {
              const bytes = Buffer.from(content, "base64");
              const digest = createHash("sha256").update(bytes).digest("hex");
              const path = `${CACHE}/${digest}.png`;
              if (!this.writes.has(path)) {
                this.writes.set(path, this.store(path, bytes, digest).finally(() => this.writes.delete(path)));
              }
              await this.writes.get(path);
              if (valid()) {
                entry.path = path;
                entry.cachedRevision = (entry.cachedRevision ?? 0) + 1;
                entry.seed = null;
                entry.canWatch = true;
                entry.retry = false;
              }
            } catch { /* Preserve the last good local image on an I/O error. */ }
          }
          if (!this.closed) { this.publish(); cx.notify(); }
        } catch { /* An unavailable machine or malformed reply is not a deletion. */ }
        finally {
          this.active--;
          for (const entry of batch) entry.busy = false;
          this.pump(cx);
        }
      });
    }
  }

  publish() {
    const next = Object.fromEntries([...this.entries.values()].filter(e => e.path).map(e => [e.key, e.path]));
    const saved = Object.fromEntries([...this.entries.values()].filter(e => e.path || e.seed)
      .map(e => [e.key, e.path ?? e.seed]));
    if (JSON.stringify(next) !== JSON.stringify(this.published)) {
      this.published = next;
      setShared(LIVE, next);
    }
    if (JSON.stringify(saved) !== JSON.stringify(this.saved)) {
      this.saved = saved;
      setShared(STORE, saved);
    }
  }

  close() {
    this.closed = true;
    this.timer?.cancel();
    for (const entry of this.entries.values()) entry.watch?.cancel();
    this.entries.clear();
  }
}
