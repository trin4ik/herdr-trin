// Pure, machine-scoped reconciliation plan. Native Spaces remain Herdr-owned;
// Projects supplies names/order and requests real background shells, never docs.
export function nativeSignature(state, spaces, machines) {
  return JSON.stringify([
    state.projects.map(p => [p.id, p.name, p.folder, p.machine, p.space, p.pending ?? null]),
    spaces.map(s => [s.id, s.machine ?? null, s.name]),
    machines.map(m => [m.name ?? null, m.layout]),
  ]);
}

export function nativeActions(state, spaces, machines) {
  const ready = machine => machines.some(m => (m.name ?? null) === machine && m.layout === "ready");
  const actions = [];
  for (const p of state.projects) {
    if (!ready(p.machine)) continue;
    const s = spaces.find(s => s.id === p.space && (s.machine ?? null) === p.machine);
    if (!s) {
      if (!p.pending) actions.push({ kind: "create", project: p.id });
    } else if (s.name !== p.name) actions.push({ kind: "rename", project: p.id, id: s.id, name: p.name });
  }
  // Indices are global/1-based, as in the shipped Spaces mod. Keep machines
  // separate and preserve the relative order of any unmanaged native spaces.
  for (const machine of new Set(state.projects.map(p => p.machine))) {
    if (!ready(machine)) continue;
    const current = spaces.filter(s => (s.machine ?? null) === machine).map(s => s.id);
    const managed = [...new Set(state.projects.filter(p => p.machine === machine && current.includes(p.space)).map(p => p.space))];
    const desired = [...managed, ...current.filter(id => !managed.includes(id))];
    const start = spaces.findIndex(s => (s.machine ?? null) === machine);
    for (let i = 0; i < desired.length; i++) {
      const from = current.indexOf(desired[i]);
      if (from === i) continue;
      actions.push({ kind: "move", id: desired[i], index: start + i + 1 });
      current.splice(i, 0, current.splice(from, 1)[0]);
    }
  }
  return actions;
}
