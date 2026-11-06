// Standalone Claude header parser: all imports stay inside the deployed plugin directory.
export const SCHEMA_VERSION = 1

const WINDOW = /^anthropic-ratelimit-unified-(5h|7d|7d-opus|7d-sonnet)-(utilization|reset|status|surpassed-threshold)$/
const STATUS = new Set(["allowed", "allowed_warning", "rejected"])
const DECIMAL = /^\d{1,15}(?:\.\d{1,9})?$/
const ISO = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/

export function matchEndpoint(provider, rawURL) {
  if (provider !== "anthropic" || typeof rawURL !== "string") return null
  try {
    const url = new URL(rawURL)
    if (url.protocol !== "https:" || url.host !== "api.anthropic.com" || url.pathname !== "/v1/messages" || url.username || url.password) return null
    return "api.anthropic.com/v1/messages"
  } catch {
    return null
  }
}

function parseValue(kind, raw) {
  if (kind === "status") return STATUS.has(raw) ? { type: "enum", value: raw } : null
  if (kind === "reset") {
    let ms
    if (/^\d{1,15}$/.test(raw)) {
      const n = Number(raw)
      ms = n >= 1e12 ? n : n * 1000
    } else if (ISO.test(raw)) {
      ms = Date.parse(raw)
    }
    if (!Number.isFinite(ms) || ms < Date.UTC(2020, 0, 1) || ms >= Date.UTC(2100, 0, 1)) return null
    return { type: "time", value: new Date(ms).toISOString() }
  }
  if (!DECIMAL.test(raw)) return null
  const n = Number(raw)
  return Number.isFinite(n) && n >= 0 && n <= 1000 ? { type: "number", value: n } : null
}

export function buildObservation({ headers, observedAt, status }) {
  const quota = []
  let scanned = 0
  for (const name of headers.keys()) {
    if (scanned++ >= 256) break
    if (typeof name !== "string") continue
    const match = WINDOW.exec(name)
    if (!match) continue
    const raw = headers.get(name)
    if (typeof raw !== "string" || raw.length < 1 || raw.length > 64) continue
    const parsed = parseValue(match[2], raw.trim())
    if (parsed) quota.push({ name, family: "subscription_candidate", ...parsed })
  }
  quota.sort((a, b) => a.name.localeCompare(b.name))
  return { observed_at: new Date(observedAt).toISOString(), status, quota }
}
