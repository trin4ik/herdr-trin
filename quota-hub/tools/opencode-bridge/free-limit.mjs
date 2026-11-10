// Passive OpenCode Console observer.
//
// The Console publishes no remaining counter for its free models: it refuses a
// request with 429 and a Retry-After header that says when the daily allowance
// comes back. That refusal is the only fact the collector can use, so this
// observer records exactly that: the provider, the time and the reset moment.
// Nothing else of the response is read or stored.
export const FREE_LIMIT_PROVIDER_ID = "opencode"
export const FREE_LIMIT_SOURCE = "opencode-http-response-hook"
export const FREE_LIMIT_ENDPOINT = "opencode.ai/inference"

const INFERENCE_HOST = "opencode.ai"
const MAX_RETRY_AFTER_SECONDS = 7 * 24 * 60 * 60

/** Matches the Console inference endpoint of any model, free or paid. */
export function matchInferenceEndpoint(rawURL) {
  if (typeof rawURL !== "string") return null
  try {
    const url = new URL(rawURL)
    if (url.protocol !== "https:") return null
    if (url.host !== INFERENCE_HOST) return null
    if (!url.pathname.startsWith("/inference/")) return null
    if (url.username || url.password || url.search) return null
    return FREE_LIMIT_ENDPOINT
  } catch {
    return null
  }
}

/** Reads Retry-After as delta-seconds or an HTTP date, bounded to a week. */
export function readRetryAfter(headers, now = Date.now()) {
  if (headers === null || typeof headers !== "object") return null
  if (typeof headers.get !== "function") return null
  const raw = headers.get("retry-after")
  if (typeof raw !== "string") return null
  const value = raw.trim()
  if (value.length < 1 || value.length > 64) return null
  if (/^\d{1,9}$/.test(value)) {
    const seconds = Number(value)
    if (!Number.isFinite(seconds) || seconds <= 0 || seconds > MAX_RETRY_AFTER_SECONDS) return null
    return now + seconds * 1000
  }
  const parsed = Date.parse(value)
  if (!Number.isFinite(parsed) || parsed <= now) return null
  if (parsed - now > MAX_RETRY_AFTER_SECONDS * 1000) return null
  return parsed
}

/**
 * Returns the only object that is ever persisted for a refused free request, or
 * null when the response is not a refusal the collector can use.
 */
export function buildFreeLimitObservation({ url, status, headers, observedAt, now = Date.now }) {
  const endpoint = matchInferenceEndpoint(url)
  if (endpoint === null) return null
  if (status !== 429) return null
  const reset = readRetryAfter(headers, now())
  if (reset === null) return null
  return {
    schema_version: 1,
    source: FREE_LIMIT_SOURCE,
    observed_at: new Date(observedAt).toISOString(),
    provider_id: FREE_LIMIT_PROVIDER_ID,
    endpoint,
    status,
    quota: [
      {
        name: "free_limit_reset",
        type: "time",
        family: "free_limit",
        value: new Date(reset).toISOString(),
      },
    ],
  }
}
