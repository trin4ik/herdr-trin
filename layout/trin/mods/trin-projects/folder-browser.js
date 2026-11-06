// One in-flight read, one latest queued intent. No timers, polling or recursion.
export const folderParent = path => path === "/" ? "/" : path.replace(/\/+$/, "").replace(/\/[^/]*$/, "") || "/";
export const folderChild = (path, name) => `${path === "/" ? "" : path}/${name}`;

export class FolderBrowser {
  constructor(read, alive = () => true) {
    this.read = read;
    this.alive = alive;
    this.version = 0;
    this.busy = false;
    this.pending = null;
    this.snapshot = null;
    this.loading = false;
    this.error = "";
    this.hidden = false;
    this.query = "";
    this.onSnapshot = () => {};
    this.onChoose = () => {};
    this.onError = () => {};
  }

  cancel() { this.version++; this.pending = null; this.loading = false; }

  reset() { this.cancel(); this.snapshot = null; this.error = ""; this.query = ""; }

  request(machine, path, cx, meta = {}, mode = "list") {
    const version = ++this.version;
    const request = { machine, path, base: this.snapshot?.path, hidden: this.hidden, query: this.query,
      ...(mode === "more" ? { cursor: this.snapshot?.nextCursor } : {}), ...(mode === "choose" ? { validate: true } : {}) };
    this.pending = { version, request, meta, mode };
    this.loading = true; this.error = "";
    cx.notify();
    if (this.busy) return;
    this.busy = true;
    cx.spawn(async cx => {
      try {
        while (this.pending && this.alive()) {
          const task = this.pending; this.pending = null;
          try {
            const next = await this.read(task.request);
            if (!this.alive()) return;
            if (task.version !== this.version) continue;
            this.loading = false;
            if (task.mode === "choose") this.onChoose(next, task.meta, cx);
            else {
              const previous = task.mode === "more" && this.snapshot?.path === next.path && this.snapshot.query === task.request.query ? this.snapshot.entries : [];
              const all = [...new Map([...previous, ...next.entries].map(e => [e.name, e])).values()];
              const entries = all.slice(0, 4000);
              this.snapshot = { ...next, query: task.request.query, entries, ...(all.length >= 4000 ? { nextCursor: null, limited: true } : {}) };
              this.onSnapshot(this.snapshot, task.meta, cx);
            }
          } catch (error) {
            if (!this.alive()) return;
            if (task.version !== this.version) continue;
            this.loading = false;
            this.error = String(error.message ?? error);
            this.onError(this.error, task.meta, cx);
          }
          if (this.alive()) cx.notify();
        }
      } finally { this.busy = false; }
    });
  }
}
