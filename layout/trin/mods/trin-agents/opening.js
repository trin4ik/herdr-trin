const CONFIRM_MS = 15000;

function programReplacement(ticket, terminal, owner, now) {
  const fg = terminal?.foreground;
  if (!Number.isFinite(fg?.pid)) return false;
  const program = terminal.program, app = terminal.report?.app;
  // The program table may still reflect a previous report. A current,
  // positively identified OpenCode foreground wins over that stale label.
  const executable = fg.argv?.[0]?.split(/[\\/]/).at(-1);
  if (executable === "opencode" || fg.name === "opencode") return false;
  const other = key => typeof key === "string" && !["opencode", "program", "agent", "unknown", ""].includes(key);
  const changed = Number.isFinite(ticket.ownerPid) && fg.pid !== ticket.ownerPid;
  if (changed && (program?.shell === true || other(program?.key) || other(app))) return true;
  return !owner && now >= ticket.expires && program?.shell === true;
}

// Acknowledged tab creation isn't proof of a running OpenCode session.
// A present tab keeps the launch lock even if session confirmation is slow.
export class BackgroundOpens {
  constructor() {
    this.pending = new Map();
    this.attached = new Map();
  }

  begin(id) {
    if (this.pending.has(id)) return null;
    const ticket = { tab: null };
    this.pending.set(id, ticket);
    return ticket;
  }

  cancel(id, ticket) {
    if (this.pending.get(id) === ticket) this.pending.delete(id);
  }

  acknowledge(ticket, reply, now) {
    ticket.tab = reply.tab;
    ticket.terminal = reply.terminal;
    ticket.expires = now + CONFIRM_MS;
  }

  focus(id) {
    return this.attached.get(id) ?? this.pending.get(id);
  }

  reconcile(tabs, attached, openCodeByTab, liveTerminals, now, panes = []) {
    this.attached = new Map([...attached.values()].filter(row => tabs.has(row.agent.tab))
      .map(row => [row.id, row.agent]));
    const claimed = new Set();
    let wakeAt = Infinity;
    for (const [id, ticket] of this.pending) {
      if (!ticket.tab) continue;
      const present = tabs.has(ticket.tab);
      const candidates = openCodeByTab.get(ticket.tab) ?? [];
      const owner = candidates.find(row => !ticket.terminal || ticket.terminal === row.address);
      ticket.terminal ??= owner?.address;
      const live = liveTerminals.get(ticket.terminal);
      const inPlace = panes.some(p => p.tab === ticket.tab && p.terminal === ticket.terminal);
      const terminalPresent = Boolean(ticket.terminal && (live || inPlace));
      ticket.sawTerminal ||= terminalPresent;
      if (this.attached.has(id) || programReplacement(ticket, live, owner, now)
        || (present && owner?.identity && owner.identity !== id)
        || (!terminalPresent && (ticket.sawTerminal || ticket.expires <= now || (!present && ticket.present)))) {
        this.pending.delete(id);
        continue;
      }
      ticket.present ||= present;
      if (!terminalPresent || (!owner && live?.program?.shell && now < ticket.expires)) wakeAt = Math.min(wakeAt, ticket.expires);
      if (present && owner) {
        ticket.hadOwner = true;
        ticket.ownerPid ??= live?.foreground?.pid;
      }
      if (ticket.hadOwner && terminalPresent) claimed.add(id);
    }
    return { claimed, wakeAt };
  }
}
