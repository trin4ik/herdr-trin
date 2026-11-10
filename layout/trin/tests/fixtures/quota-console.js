// Synthetic OpenCode Console fixture. No real account, balance or request is used.
// The Console has no percentage for its free models: it refuses them with 429 and
// a reset time. The row therefore shows the balance, and the details show counters.
export const CONSOLE_NOW = "2026-11-04T10:00:00Z";
const after = minutes => new Date(Date.parse(CONSOLE_NOW) + minutes * 60000).toISOString();

export const consoleSubscription = {
  id: "opencode-console",
  provider: "opencode",
  name: "OpenCode Console",
  plan: null,
  accountEmail: null,
  source: "opencode_console",
  primaryWindowId: "free_models",
  observedAt: "2026-11-04T09:58:00Z",
  lastAttemptAt: "2026-11-04T09:59:00Z",
  nextAttemptAt: "2026-11-04T10:03:00Z",
  staleAfter: "2026-11-04T10:28:00Z",
  stale: false,
  status: "ok",
  error: null,
  balances: [{ label: "Баланс кредитов", value: 12.5, unit: "USD" }],
  windows: [
    {
      id: "free_models", metric: "quota", unit: "percent", name: "Бесплатные модели",
      label: "Бесплатные модели", usedPercent: 100, remainingPercent: 0, used: null, limit: null,
      remaining: null, counters: {}, limited: null, observedAt: "2026-11-04T09:58:00Z",
      startedAt: null, resetsAt: after(840), durationSeconds: null, period: null, scope: null,
    },
    {
      id: "model_minimax_m3_1_flash_free", metric: "counter", unit: "requests",
      name: "minimax-m3.1-flash-free", label: "minimax-m3.1-flash-free", usedPercent: null,
      remainingPercent: null, used: 121, limit: null, remaining: null,
      counters: { input_tokens: 314460, output_tokens: 71236, cache_read_tokens: 13490246, cost_usd: 0 },
      limited: null, observedAt: null, startedAt: null, resetsAt: null, durationSeconds: null,
      period: null, scope: "free",
    },
    {
      id: "go_weekly", metric: "quota", unit: "percent", name: "Go · Неделя", label: "Go · Неделя",
      usedPercent: 12, remainingPercent: 88, used: null, limit: null, remaining: null, counters: {},
      limited: false, observedAt: "2026-11-04T09:58:00Z", startedAt: null, resetsAt: after(4320),
      durationSeconds: 604800, period: "week", scope: null,
    },
  ],
};

// The same workspace in the raw quota-hub schema v1 shape.
export const consolePayload = {
  schema_version: 1,
  generated_at: CONSOLE_NOW,
  subscriptions: [{
    id: "opencode-console",
    provider: "opencode",
    product: "opencode_console",
    source: "opencode_console",
    plan: null,
    account_email: null,
    status: "ok",
    stale: false,
    stale_after: "2026-11-04T10:28:00Z",
    last_attempt_at: "2026-11-04T09:59:00Z",
    last_success_at: "2026-11-04T09:58:00Z",
    next_attempt_at: "2026-11-04T10:03:00Z",
    error: null,
    quotas: [
      {
        id: "free_models", metric: "quota", unit: "percent", used_percent: 100, remaining_percent: 0,
        label: "Бесплатные модели", observed_at: "2026-11-04T09:58:00Z",
        window: { kind: "unknown", period: null, duration_seconds: null, started_at: null, reset_at: after(840) },
        authority: "provider_reported",
      },
      {
        id: "model_minimax_m3_1_flash_free", metric: "counter", unit: "requests", used: 121,
        label: "minimax-m3.1-flash-free", scope: "free",
        counters: { input_tokens: 314460, output_tokens: 71236, cache_read_tokens: 13490246, cost_usd: 0 },
        window: { kind: "unknown", period: null, duration_seconds: null, started_at: null, reset_at: null },
        authority: "provider_reported",
      },
      {
        id: "go_weekly", metric: "quota", unit: "percent", used_percent: 12, remaining_percent: 88,
        label: "Go · Неделя", limited: false, observed_at: "2026-11-04T09:58:00Z",
        window: { kind: "unknown", period: "week", duration_seconds: 604800, started_at: null, reset_at: after(4320) },
        authority: "provider_reported",
      },
    ],
    metrics: [{ id: "credits_balance", metric: "balance", unit: "USD", value: 12.5, authority: "provider_reported" }],
  }],
};
