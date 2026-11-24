// The OpenCode and Herdr event streams are independent. An active session can
// be delivered before Herdr has published the report of the terminal it owns.
export const DISCOVERY_GRACE_MS = 1000;
const OWNERSHIP_GRACE_MS = 4000;
const MAX_REMEMBERED = 256;

export class BackgroundAdmission {
  constructor(seen = []) {
    this.pending = new Map();
    this.owners = new Map();
    this.seen = new Set(seen.slice(-MAX_REMEMBERED));
  }

  remember(owners, liveTerminals) {
    const known = new Map();
    let changed = false;
    for (const [id, addresses] of owners) {
      this.owners.delete(id);
      this.owners.set(id, { addresses, detached: false });
      for (const address of addresses) known.set(address, id);
      if (!this.seen.has(id)) {
        this.seen.add(id);
        changed = true;
      }
    }
    for (const [id, owner] of this.owners) {
      // An idle tab can omit its report. Only declare it detached when all
      // last confirmed terminals have gone or now report a different ID.
      owner.detached ||= [...owner.addresses].every(address => !liveTerminals.has(address)
        || (known.has(address) && known.get(address) !== id));
    }
    while (this.owners.size > MAX_REMEMBERED) this.owners.delete(this.owners.keys().next().value);
    while (this.seen.size > MAX_REMEMBERED) this.seen.delete(this.seen.values().next().value);
    return changed;
  }

  update(incoming, claimed, unresolvedMachines, now) {
    const next = new Map(), rows = [];
    let wakeAt = Infinity;
    for (const row of incoming) {
      const id = row.address, machine = row.machine ?? null;
      const owner = this.owners.get(id);
      if (claimed.has(id) || next.has(id) || (owner && !owner.detached)
        || (row.status !== "working" && this.seen.has(id))) continue;
      const entry = this.pending.get(id) ?? { started: now, shown: false };
      next.set(id, entry);
      // Don't flap already visible independent background work when a
      // different tab starts. Ownership uncertainty has a finite deadline.
      if (!entry.shown) {
        const grace = unresolvedMachines.has(machine) && !owner?.detached ? OWNERSHIP_GRACE_MS : DISCOVERY_GRACE_MS;
        const due = entry.started + grace;
        if (now >= due) entry.shown = true;
        else wakeAt = Math.min(wakeAt, due);
      }
      if (entry.shown) rows.push(row);
    }
    this.pending = next;
    return { rows, wakeAt };
  }
}
