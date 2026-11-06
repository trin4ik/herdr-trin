import { normalizeSnapshot } from "./data.js";

export const REFRESH_MS = 60000;

export class QuotaMonitor {
  constructor(request) {
    this.request = request;
    this.subscriptions = [];
    this.generatedAt = null;
    this.fetchedAt = null;
    this.error = null;
    this.busy = false;
    this.closed = false;
  }

  refresh(cx) {
    if (this.closed || this.busy) return;
    this.busy = true;
    cx.spawn(async cx => {
      try {
        const next = normalizeSnapshot(await this.request());
        if (this.closed) return;
        if (this.generatedAt && Date.parse(next.generatedAt) < Date.parse(this.generatedAt)) {
          throw new Error("Сервис вернул более старый снимок; показаны предыдущие данные");
        }
        const previous = new Map(this.subscriptions.map(s => [s.id, s]));
        this.subscriptions = next.subscriptions.map(s => {
          const old = previous.get(s.id);
          if (!old || !(s.error || s.stale || s.status !== "ok")) return s;
          // A failed provider refresh must not erase last-known usage. A valid
          // successful empty snapshot, including removed accounts, stays empty.
          const windows = s.windows.length ? s.windows.map(w => w.usedPercent == null
            ? old.windows.find(p => p.id === w.id) ?? w : w) : old.windows;
          return { ...s, windows, primaryWindowId: s.primaryWindowId ?? old.primaryWindowId,
            observedAt: s.observedAt ?? old.observedAt };
        });
        this.generatedAt = next.generatedAt;
        this.fetchedAt = new Date().toISOString();
        this.error = null;
      } catch (error) {
        if (!this.closed) this.error = { message: error.message || "Не удалось получить квоты", status: error.status,
          at: new Date().toISOString() };
      } finally {
        this.busy = false;
        if (!this.closed) cx.notify();
      }
    });
  }

  close() { this.closed = true; }
}
