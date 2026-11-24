// The OpenCode and Herdr event streams are independent. An active session can
// be delivered before Herdr has published the report of the terminal it owns.
export const DISCOVERY_GRACE_MS = 1000;

export class BackgroundAdmission {
  constructor() {
    this.pending = new Map();
  }

  update(incoming, claimed, unresolvedMachines, now) {
    const next = new Map(), rows = [];
    let wakeAt = Infinity;
    for (const row of incoming) {
      const id = row.address, machine = row.machine ?? null;
      if (claimed.has(id) || next.has(id)) continue;
      const entry = this.pending.get(id) ?? { due: null, shown: false };
      next.set(id, entry);
      // Don't flap already visible independent background work when a
      // different tab starts. Hold new candidates while ownership is unclear.
      if (!entry.shown) {
        if (unresolvedMachines.has(machine)) entry.due = null;
        else {
          entry.due ??= now + DISCOVERY_GRACE_MS;
          if (now >= entry.due) entry.shown = true;
          else wakeAt = Math.min(wakeAt, entry.due);
        }
      }
      if (entry.shown) rows.push(row);
    }
    this.pending = next;
    return { rows, wakeAt };
  }
}
