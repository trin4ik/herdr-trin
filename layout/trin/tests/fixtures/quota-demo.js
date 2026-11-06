// Historical synthetic test fixture. Never imported by the live quota mod.
// Kimi's main usage is intentionally 94% to show the muted red warning.
export const DEMO_NOW = "2026-11-04T10:00:00Z";
const after = minutes => new Date(Date.parse(DEMO_NOW) + minutes * 60000).toISOString();
const limit = (id, label, usedPercent, minutes) => ({ id, label, usedPercent, resetsAt: after(minutes) });

export const subscriptions = [
  { id: "chatgpt-personal", provider: "openai", name: "ChatGPT", primaryWindowId: "weekly",
    observedAt: "2026-11-04T09:58:00Z",
    windows: [limit("weekly", "Неделя", 32, 3120)] },
  { id: "claude-personal", provider: "anthropic", name: "Claude", primaryWindowId: "five-hour",
    observedAt: "2026-11-04T09:58:00Z",
    windows: [limit("five-hour", "5 часов", 72, 138), limit("weekly", "Неделя", 41, 4320)] },
  { id: "ollama-personal", provider: "ollama", name: "Ollama", primaryWindowId: "five-hour",
    observedAt: "2026-11-04T09:58:00Z",
    windows: [limit("five-hour", "5 часов", 18, 192), limit("weekly", "Неделя", 36, 7200), limit("monthly", "Месяц", 24, 24480)] },
  { id: "kimi-personal", provider: "kimi", name: "Kimi", primaryWindowId: "five-hour",
    observedAt: "2026-11-04T09:42:00Z", lastAttemptAt: "2026-11-04T09:59:00Z",
    error: { status: 503, message: "Service Unavailable" },
    windows: [limit("five-hour", "5 часов", 94, 64), limit("monthly", "Месяц", 67, 17280)] },
];
