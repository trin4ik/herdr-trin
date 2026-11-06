export function usage(window) {
  const value = window?.usedPercent;
  return typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
}

export function remaining(window) {
  const used = usage(window);
  const left = window?.remainingPercent;
  if (typeof left === "number" && Number.isFinite(left) && left >= 0 && left <= 100) return left;
  return used == null ? null : 100 - used;
}

export const percent = value => value == null ? "—" : `${Number(value.toFixed(2))}%`;
export const compactPercent = value => value == null ? "—" : `${Math.round(value)}%`;

// Presentation aliases only: never infer a subscription tier from its quotas.
export function planLabel(plan) {
  if (typeof plan !== "string" || !plan.trim()) return null;
  const value = plan.trim();
  return ({ prolite: "Pro Lite", pro_lite: "Pro Lite", "pro-lite": "Pro Lite",
    plus: "Plus", pro: "Pro", max: "Max", ultra: "Ultra", free: "Free",
    team: "Team", teams: "Teams", enterprise: "Enterprise", go: "Go" })[value.toLowerCase()] ?? value;
}

export function primaryWindow(subscription) {
  return subscription.windows.find(w => w.id === subscription.primaryWindowId)
    ?? subscription.windows[0];
}

export function quotaColor(t, value) {
  return value == null ? t.dim : value >= 90 ? t.ansi1 : value >= 60 ? t.ansi3 : t.ansi2;
}

// A passed reset never fabricates renewed quota: wait for the collector snapshot.
export function resetLabel(resetsAt, now) {
  if (!resetsAt) return "↻ —";
  const minutes = Math.ceil((Date.parse(resetsAt) - Date.parse(now)) / 60000);
  if (!Number.isFinite(minutes)) return "↻ —";
  if (minutes <= 0) return "↻ ждём";
  const days = Math.floor(minutes / 1440), hours = Math.floor(minutes % 1440 / 60), rest = minutes % 60;
  return `↻ ${days ? `${days}д${hours ? ` ${hours}ч` : ""}` : `${hours ? `${hours}ч` : ""}${hours && rest ? " " : ""}${rest ? `${rest}м` : ""}`}`;
}

export function updatedLabel(timestamp) {
  if (!timestamp || !Number.isFinite(Date.parse(timestamp))) return "неизвестно";
  // Moscow uses UTC+3 year-round; UTC getters keep display independent of the host timezone.
  const date = new Date(Date.parse(timestamp) + 3 * 60 * 60 * 1000), pad = n => String(n).padStart(2, "0");
  return `${pad(date.getUTCDate())}.${pad(date.getUTCMonth() + 1)}.${date.getUTCFullYear()}, ${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())} МСК`;
}

export function hasWarning(subscription, now) {
  return Boolean(subscription.error || subscription.stale || (subscription.status && subscription.status !== "ok")
    || (subscription.staleAfter && Date.parse(now) >= Date.parse(subscription.staleAfter)));
}

export function quotaTooltip(subscription, now, context = {}) {
  const primary = primaryWindow(subscription);
  return [
    `${subscription.name}${planLabel(subscription.plan) ? ` · ${planLabel(subscription.plan)}` : ""}`,
    `В строке: ${primary?.label ?? "нет окна"} · расход ${percent(usage(primary))} · осталось ${percent(remaining(primary))} · сброс ${resetLabel(primary?.resetsAt, now)}`,
    ...subscription.windows.filter(w => w !== primary)
      .map(w => `${w.label}: расход ${percent(usage(w))} · осталось ${percent(remaining(w))} · сброс ${resetLabel(w.resetsAt, now)}`),
    `Последнее успешное обновление: ${updatedLabel(subscription.observedAt)}`,
    ...(context.fetchedAt ? [`Снимок получен Herdr: ${updatedLabel(context.fetchedAt)}`] : []),
    ...(subscription.nextAttemptAt ? [`Следующая попытка сборщика: ${updatedLabel(subscription.nextAttemptAt)}`] : []),
    ...(subscription.balances ?? []).map(b => `${b.label}: ${b.value} ${b.unit}`),
    ...(subscription.error ? [
      "",
      `Ошибка${subscription.error.status != null ? ` HTTP ${subscription.error.status}` : ""}${subscription.error.code ? ` (${subscription.error.code})` : ""}: ${subscription.error.message || "запрос не выполнен"}`,
      `Последняя попытка: ${updatedLabel(subscription.lastAttemptAt)}`,
      subscription.windows.some(w => remaining(w) != null)
        ? "Показаны последние известные данные; они могут быть устаревшими."
        : "Данные недоступны; остаток неизвестен.",
    ] : []),
    ...(!subscription.error && hasWarning(subscription, now) ? ["", "Данные устарели или сборщик не подтвердил успешное обновление."] : []),
    ...(context.error ? ["", `Связь с сервисом: ${context.error.message}`,
      `Неудачная попытка Herdr: ${updatedLabel(context.error.at)}`,
      "Показан последний полученный снимок; значения могут быть устаревшими."] : []),
  ].join("\n");
}
