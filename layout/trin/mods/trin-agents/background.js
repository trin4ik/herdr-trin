// The OpenCode and Herdr event streams are independent. An active session can
// be delivered before Herdr has published the report of the terminal it owns.
export const DISCOVERY_GRACE_MS = 1000;
const MAX_REMEMBERED = 256;

export class BackgroundAdmission {
  constructor() {
    this.pending = new Map();
    this.owners = new Map();
  }

  remember(owners, liveTerminals) {
    for (const [id, addresses] of owners) {
      this.owners.delete(id);
      this.owners.set(id, { addresses, detached: false });
    }
    for (const owner of this.owners.values()) {
      // An idle tab can omit its report. Only declare it detached when all
      // of its last confirmed terminals have actually gone from the layout.
      owner.detached ||= [...owner.addresses].every(address => !liveTerminals.has(address));
    }
    while (this.owners.size > MAX_REMEMBERED) this.owners.delete(this.owners.keys().next().value);
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
        if (unresolvedMachines.has(machine) && !this.owners.get(id)?.detached) entry.due = null;
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
