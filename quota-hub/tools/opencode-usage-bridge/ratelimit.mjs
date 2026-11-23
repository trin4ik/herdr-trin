import * as fs from "node:fs/promises"
import os from "node:os"
import { dirname, isAbsolute, join } from "node:path"

// Claude Code harness (provider claude-agent) passes Claude's own rate_limit_event of
// every answer as step native.rateLimit. This is the only Claude quota source: the
// plugin and quota-hub never ask Anthropic for limits.
export const RATE_LIMIT_SOURCE = "opencode-claude-agent"
export const DEFAULT_CLAUDE_PROVIDERS = ["claude-agent"]
const prefix = "anthropic-ratelimit-unified-"
const windowID = /^(five_hour|seven_day)(?:_([a-z0-9]{1,32}))?$/
const short = { five_hour: "5h", seven_day: "7d" }

export function claudeQuotaFile(home = os.homedir()) {
  return join(home, ".local", "state", "quota-hub", "bridge", "claude.latest.json")
}

const seconds = (value) => Number.isSafeInteger(value) && value > 0 && value < 1e11
  ? new Date(value * 1000).toISOString() : undefined
const fraction = (value) => typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 10

// toObservation maps one rate_limit_info to the bridge file schema read by quota-hub.
export function toObservation(info, created) {
  try {
    if (!info || typeof info !== "object" || Array.isArray(info)) return null
    if (!Number.isFinite(created) || !Number.isFinite(new Date(created).getTime())) return null
    const windows = new Map()
    const add = (id, utilization, resetsAt) => {
      const m = typeof id === "string" ? windowID.exec(id) : null
      if (!m || windows.has(id) || !fraction(utilization)) return
      windows.set(id, { token: short[m[1]] + (m[2] ? `_${m[2]}` : ""), utilization, reset: seconds(resetsAt) })
    }
    const unified = info.unifiedWindows
    if (unified && typeof unified === "object" && !Array.isArray(unified)) {
      for (const [id, w] of Object.entries(unified)) add(id, w?.utilization, w?.resetsAt)
    }
    add(info.rateLimitType, info.utilization, info.resetsAt)
    if (!windows.size) return null
    const quota = []
    for (const w of windows.values()) {
      quota.push({ name: `${prefix}${w.token}-utilization`, type: "number", value: w.utilization })
      if (w.reset) quota.push({ name: `${prefix}${w.token}-reset`, type: "time", value: w.reset })
    }
    return {
      schema_version: 1, source: RATE_LIMIT_SOURCE, provider_id: "anthropic",
      observed_at: new Date(created).toISOString(),
      status: info.status === "rejected" ? 429 : 200, quota,
    }
  } catch {
    return null
  }
}

export function claudeProviders(option) {
  return Array.isArray(option) && option.length > 0 && option.every((p) => typeof p === "string" && p.length > 0 && p.length <= 64)
    ? option : DEFAULT_CLAUDE_PROVIDERS
}

export function quotaFileOption(option) {
  return typeof option === "string" && isAbsolute(option) ? option : claudeQuotaFile()
}

// writeObservation replaces the file atomically; older observations never replace newer ones.
export async function writeObservation(file, observation, state) {
  const at = Date.parse(observation.observed_at)
  if (state.latest !== undefined && at < state.latest) return false
  state.latest = at
  await fs.mkdir(dirname(file), { recursive: true, mode: 0o700 })
  const temporary = `${file}.${process.pid}.${Math.random().toString(36).slice(2)}.tmp`
  try {
    await fs.writeFile(temporary, `${JSON.stringify(observation)}\n`, { flag: "wx", mode: 0o600 })
    await fs.rename(temporary, file)
  } finally {
    await fs.unlink(temporary).catch(() => {})
  }
  return true
}
