// quota-hub schema v1 -> presentation data. No provider requests here.
const PROVIDERS = {
  codex: { provider: "openai", name: "ChatGPT / Codex", order: 0 },
  openai: { provider: "openai", name: "ChatGPT / Codex", order: 0 },
  claude: { provider: "anthropic", name: "Claude", order: 1 },
  anthropic: { provider: "anthropic", name: "Claude", order: 1 },
  ollama: { provider: "ollama", name: "Ollama", order: 2 },
  kimi: { provider: "kimi", name: "Kimi", order: 3 },
  opencode: { provider: "opencode", name: "OpenCode Console", order: 4 },
};
const validPercent = value => typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 100;
const validAmount = value => typeof value === "number" && Number.isFinite(value) && value >= 0;
const date = value => typeof value === "string" && Number.isFinite(Date.parse(value)) ? value : null;
const text = value => typeof value === "string" ? value.slice(0, 300) : null;

// Counter values are plain numbers with a known key; anything else is ignored.
const COUNTER_KEY = /^[a-z][a-z0-9_]{0,39}$/;
function normalizeCounters(raw) {
  if (raw === null || typeof raw !== "object") return {};
  const out = {};
  for (const [key, value] of Object.entries(raw)) {
    if (!COUNTER_KEY.test(key) || !validAmount(value)) continue;
    out[key] = value;
  }
  return out;
}

export function normalizeError(error) {
  if (!error) return null;
  if (typeof error === "string") return { message: error.slice(0, 300) };
  const status = [error.http_status, error.upstream_status, error.status_code, error.status]
    .find(value => Number.isInteger(value) && value >= 100 && value <= 599);
  return { status, code: text(error.code), message: text(error.message) || "Обновление недоступно" };
}

export function windowLabel(quota) {
  const w = quota.window ?? {};
  const seconds = w.duration_seconds;
  let label;
  if (w.period === "month") label = "Месяц";
  else if (w.period === "week" || seconds === 604800) label = "Неделя";
  else if (w.period === "day" || seconds === 86400) label = "Сутки";
  else if (typeof seconds === "number" && seconds > 0 && seconds % 3600 === 0) label = `${seconds / 3600} часов`;
  else if (typeof seconds === "number" && seconds > 0) label = `${Math.round(seconds / 60)} мин`;
  else if (quota.id === "session") label = "Сессия";
  else label = text(quota.id) || "Лимит";
  const scope = quota.scope === "code" ? "код" : quota.scope === "total" ? "всего" : text(quota.scope);
  return scope ? `${label} · ${scope}` : label;
}

function normalizeQuota(q) {
  if (!q || typeof q.id !== "string" || !q.id) throw new Error("Некорректное окно лимита");
  const metric = q.metric === "counter" ? "counter" : "quota";
  let used = validPercent(q.used_percent) ? q.used_percent : null;
  const left = validPercent(q.remaining_percent) ? q.remaining_percent : null;
  if (used == null && left != null) used = 100 - left;
  const name = typeof q.label === "string" && q.label.trim() ? q.label.trim().slice(0, 120) : null;
  return { id: q.id, metric,
    unit: typeof q.unit === "string" && q.unit.trim() ? q.unit.trim().slice(0, 24) : "percent",
    name, label: windowLabel(q), usedPercent: used, remainingPercent: left,
    used: validAmount(q.used) ? q.used : null,
    limit: validAmount(q.limit) ? q.limit : null,
    remaining: validAmount(q.remaining) ? q.remaining : null,
    counters: normalizeCounters(q.counters),
    limited: typeof q.limited === "boolean" ? q.limited : null,
    observedAt: date(q.observed_at),
    startedAt: date(q.window?.started_at), resetsAt: date(q.window?.reset_at), durationSeconds: q.window?.duration_seconds ?? null,
    period: q.window?.period ?? null,
    scope: typeof q.scope === "string" ? q.scope.slice(0, 80) : null };
}

export function normalizeSnapshot(payload) {
  if (payload?.schema_version !== 1 || !date(payload.generated_at) || !Array.isArray(payload.subscriptions)) {
    throw new Error("Неподдерживаемый ответ сервиса квот (ожидается schema_version: 1)");
  }
  const ids = new Set();
  const subscriptions = payload.subscriptions.map(s => {
    if (!s || typeof s.id !== "string" || !s.id || ids.has(s.id) || typeof s.provider !== "string"
      || (s.quotas != null && !Array.isArray(s.quotas))) throw new Error("Некорректный список подписок");
    ids.add(s.id);
    const identity = PROVIDERS[s.provider] ?? { provider: s.provider, name: s.provider, order: 5 };
    const windows = (s.quotas ?? []).map(normalizeQuota);
    if (new Set(windows.map(w => w.id)).size !== windows.length) throw new Error("Повторяющиеся окна лимитов");
    // The row shows the window that matters most: for the Console the refused
    // free models, then the Go allowance, then the usual session/weekly rules.
    let primary = null;
    if (identity.provider === "opencode") {
      primary = windows.find(w => w.id === "free_models") ?? windows.find(w => w.id === "go_weekly")
        ?? windows.find(w => w.id === "go_monthly") ?? windows.find(w => w.id === "go_rolling") ?? null;
    } else if (identity.provider === "openai") {
      primary = windows.find(w => w.period === "week" || w.durationSeconds === 604800) ?? null;
    }
    primary = primary ?? windows.find(w => w.durationSeconds === 18000) ?? windows.find(w => w.id === "session") ?? windows[0];
    // Optional service extension: never derive identity from IDs, sources or credentials.
    const accountEmail = typeof s.account_email === "string" && !/[\x00-\x1f\x7f]/.test(s.account_email)
      ? text(s.account_email.trim()) || null : null;
    return { id: s.id, ...identity, plan: text(s.plan), accountEmail, source: text(s.source),
      primaryWindowId: primary?.id, windows,
      observedAt: date(s.last_success_at), lastAttemptAt: date(s.last_attempt_at),
      nextAttemptAt: date(s.next_attempt_at), staleAfter: date(s.stale_after),
      stale: s.stale === true, status: text(s.status) || "unknown", error: normalizeError(s.error),
      balances: Array.isArray(s.metrics) ? s.metrics.filter(m => m?.metric === "balance" && typeof m.value === "number" && Number.isFinite(m.value))
        .map(m => ({ label: m.id === "credits_balance" ? "Баланс кредитов" : m.id === "purchased_balance" ? "Купленный баланс"
          : m.id === "credit_limit" ? "Лимит кредитов" : text(m.id) || "Баланс", value: m.value, unit: text(m.unit) || "" })) : [],
    };
  }).sort((a, b) => a.order - b.order || a.id.localeCompare(b.id));
  return { generatedAt: payload.generated_at, subscriptions };
}
