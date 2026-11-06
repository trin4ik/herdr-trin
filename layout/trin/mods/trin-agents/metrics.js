export function formatTokens(value) {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return "—";
  for (const [size, suffix] of [[1e9, "b"], [1e6, "m"], [1e3, "k"]]) {
    if (value >= size * 0.9995) return `${Number((value / size).toFixed(value / size < 100 ? 1 : 0))}${suffix}`;
  }
  return String(Math.round(value));
}

export function formatActiveTime(ms) {
  if (typeof ms !== "number" || !Number.isFinite(ms) || ms < 0) return "—";
  const seconds = Math.floor(ms / 1000), minutes = Math.floor(seconds / 60);
  const tail = `${String(minutes % 60).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`;
  return minutes >= 60 ? `${Math.floor(minutes / 60)}:${tail}` : tail;
}

export function modelLabel(id) {
  if (!id) return "—";
  return id.replace(/^claude-/, "").replace(/^gpt-/, "")
    .replace(/(\d)-(\d)(?=-|$)/g, "$1.$2")
    .replace(/-/g, " ").replace(/\b(opus|sonnet|haiku|sol|fast|mini|nano|terra)\b/g, s => s[0].toUpperCase() + s.slice(1));
}

export function activeTime(meta, now) {
  if (!meta || !meta.durationReady || meta.durationMs == null) return null;
  const live = meta.durationRunning && !meta.stale && !meta.durationStale
    ? Math.max(0, now - meta.durationAt) : 0;
  return meta.durationMs + live;
}

// History seeds the total once. From then on Herdr's session status owns the
// clock, not the presence of an open assistant step or freshness of SSE data.
// In particular, a newer/lower API snapshot must never reset the live anchor.
export class WorkingClock {
  constructor() {
    this.sessions = new Map();
  }

  sync(agents, details, now) {
    const live = new Set();
    for (const agent of agents) {
      live.add(agent.address);
      const meta = details(agent);
      let state = this.sessions.get(agent.address);
      // A rename temporarily hides title-matched API metadata. Keep a known
      // identity through that gap; only a confirmed new session can replace it.
      const different = state && (state.id
        ? Boolean(meta?.sessionID && state.id !== meta.sessionID)
        : state.title !== agent.title);
      if (!state || different) {
        state = { id: meta?.sessionID, title: agent.title, totalMs: 0,
          at: now, working: false, seeded: false };
        this.sessions.set(agent.address, state);
      }
      if (state.working) state.totalMs += Math.max(0, now - state.at);
      state.at = now;
      state.working = agent.status === "working";
      state.title = agent.title;
      state.id ??= meta?.sessionID;
      if (!state.seeded && meta?.durationReady && Number.isFinite(meta.durationMs)) {
        state.totalMs = Math.max(state.totalMs, activeTime(meta, now));
        state.seeded = true;
      }
    }
    for (const address of this.sessions.keys()) if (!live.has(address)) this.sessions.delete(address);
  }

  value(address) {
    const state = this.sessions.get(address);
    return state && (state.seeded || state.working || state.totalMs > 0) ? state.totalMs : null;
  }
}
