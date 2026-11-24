export function parseOrder(raw) {
  try {
    const ids = JSON.parse(raw);
    return Array.isArray(ids) ? [...new Set(ids.filter(id => typeof id === "string" && id))] : [];
  } catch {
    return [];
  }
}

export function orderedSubscriptions(subscriptions, order) {
  const rank = new Map(order.map((id, index) => [id, index]));
  return [...subscriptions].sort((a, b) => (rank.get(a.id) ?? rank.size) - (rank.get(b.id) ?? rank.size));
}

export function moveSubscription(subscriptions, order, source, target, after) {
  if (source === target) return order;
  const visible = orderedSubscriptions(subscriptions, order).map(s => s.id);
  if (!visible.includes(source) || !visible.includes(target)) return order;
  const moved = visible.filter(id => id !== source);
  moved.splice(moved.indexOf(target) + Number(after), 0, source);
  if (moved.every((id, i) => id === visible[i])) return order;
  // Keep saved positions for accounts absent from this snapshot.
  const showing = new Set(visible);
  let i = 0;
  return [...new Set([...order, ...visible])].map(id => showing.has(id) ? moved[i++] : id);
}
