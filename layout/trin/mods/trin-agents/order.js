export function parseOrder(raw) {
  try {
    const ids = JSON.parse(raw);
    return Array.isArray(ids) ? [...new Set(ids.filter(id => typeof id === "string" && id))] : [];
  } catch {
    return [];
  }
}

export function orderedSessions(rows, order) {
  const rank = new Map(order.map((id, i) => [id, i]));
  return [...rows].sort((a, b) => (rank.get(a.id) ?? rank.size) - (rank.get(b.id) ?? rank.size));
}

export function moveSession(rows, order, source, target, after) {
  const visible = orderedSessions(rows, order).map(row => row.id);
  if (source === target || !visible.includes(source) || !visible.includes(target)) return order;
  const moved = visible.filter(id => id !== source);
  moved.splice(moved.indexOf(target) + Number(after), 0, source);
  if (moved.every((id, i) => id === visible[i])) return order;
  // A finished background session or a disconnected machine can return.
  const showing = new Set(visible);
  let i = 0;
  return [...new Set([...order, ...visible])].map(id => showing.has(id) ? moved[i++] : id);
}
