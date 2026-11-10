export function usage(window) {
  if (window?.metric === "counter") return null;
  const value = window?.usedPercent;
  return typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
}

// A counter window carries absolute values (requests, tokens, money), not a share.
export function isCounter(window) {
  return window?.metric === "counter";
}

// The provider may name a window; the derived label is the fallback.
export function windowTitle(window) {
  return window?.name ?? window?.label ?? "Лимит";
}

// A window the provider reports as uncapped must not look like a full bar.
export function unlimited(window) {
  return window?.limited === false;
}

// The scope names a pool or a class of models; only a few have a known word.
export function scopeLabel(scope) {
  if (typeof scope !== "string" || !scope.trim()) return null;
  const known = { free: "бесплатно", code: "код", total: "всего" };
  return known[scope.trim().toLowerCase()] ?? scope.trim();
}

const plural = (value, one, few, many) => {
  const mod10 = value % 10, mod100 = value % 100;
  if (mod10 === 1 && mod100 !== 11) return one;
  if (mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14)) return few;
  return many;
};

export function formatCount(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  const rounded = Math.round(value);
  return `${rounded.toLocaleString("ru-RU")} ${plural(rounded, "запрос", "запроса", "запросов")}`;
}

export function formatTokens(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  if (value < 1000) return `${Math.round(value)}`;
  if (value < 1e6) return `${(value / 1000).toFixed(value < 10000 ? 1 : 0)}k`;
  if (value < 1e9) return `${(value / 1e6).toFixed(value < 1e7 ? 1 : 0)}M`;
  return `${(value / 1e9).toFixed(1)}B`;
}

export function formatMoney(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  const digits = value === 0 ? 2 : value < 0.01 ? 4 : value < 1 ? 3 : 2;
  return `$${value.toFixed(digits)}`;
}

// The row of a subscription without a percentage (a pay-as-you-go balance)
// shows the balance instead of a dash.
export function balanceText(balance) {
  if (!balance || typeof balance.value !== "number" || !Number.isFinite(balance.value)) return "—";
  if (balance.unit === "USD") return formatMoney(balance.value);
  return `${balance.value} ${balance.unit ?? ""}`.trim();
}

// The extra values a counter window carries, in a stable order.
export function counterRows(window) {
  const counters = window?.counters ?? {};
  const rows = [];
  if (typeof window?.used === "number") rows.push(["Запросы", formatCount(window.used)]);
  if (typeof counters.input_tokens === "number") rows.push(["Входные токены", formatTokens(counters.input_tokens)]);
  if (typeof counters.output_tokens === "number") rows.push(["Выходные токены", formatTokens(counters.output_tokens)]);
  if (typeof counters.cache_read_tokens === "number") rows.push(["Кэш, чтение", formatTokens(counters.cache_read_tokens)]);
  if (typeof counters.cost_usd === "number") rows.push(["Стоимость", formatMoney(counters.cost_usd)]);
  if (typeof counters.limit_usd === "number") rows.push(["Лимит бюджета", formatMoney(counters.limit_usd)]);
  if (typeof counters.spent_usd === "number") rows.push(["Израсходовано", formatMoney(counters.spent_usd)]);
  return rows;
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

// One line per window: a share for a quota, absolute values for a counter.
export function windowSummary(window, now) {
  if (isCounter(window)) return counterRows(window).map(([label, value]) => `${label.toLowerCase()} ${value}`).join(" · ");
  if (unlimited(window)) return `без лимита · сброс ${resetLabel(window.resetsAt, now)}`;
  return `расход ${percent(usage(window))} · осталось ${percent(remaining(window))} · сброс ${resetLabel(window.resetsAt, now)}`;
}

export function quotaTooltip(subscription, now, context = {}) {
  const primary = primaryWindow(subscription);
  return [
    `${subscription.name}${planLabel(subscription.plan) ? ` · ${planLabel(subscription.plan)}` : ""}`,
    `В строке: ${primary ? windowTitle(primary) : "нет окна"} · ${primary ? windowSummary(primary, now) : "нет данных"}`,
    ...subscription.windows.filter(w => w !== primary)
      .map(w => `${windowTitle(w)}: ${windowSummary(w, now)}`),
    `Последнее успешное обновление: ${updatedLabel(subscription.observedAt)}`,
    ...(context.fetchedAt ? [`Снимок получен Herdr: ${updatedLabel(context.fetchedAt)}`] : []),
    ...(subscription.nextAttemptAt ? [`Следующая попытка сборщика: ${updatedLabel(subscription.nextAttemptAt)}`] : []),
    ...(subscription.balances ?? []).map(b => `${b.label}: ${balanceText(b)}`),
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
