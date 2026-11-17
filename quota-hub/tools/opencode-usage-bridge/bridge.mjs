import * as fs from "node:fs/promises"
import os from "node:os"
import { isAbsolute, join } from "node:path"

export const PLUGIN_ID = "quota-hub.usage-bridge"
const types = ["session.step.started", "session.step.ended", "session.step.failed", "session.usage.recorded"]
const text = (value) => typeof value === "string" && value.length > 0 && value.length <= 256
const optional = (value) => typeof value === "string" ? value : null

export function baseDir(env = process.env, home = os.homedir()) {
  return join(typeof env.XDG_STATE_HOME === "string" && isAbsolute(env.XDG_STATE_HOME)
    ? env.XDG_STATE_HOME : join(home, ".local", "state"), "quota-usage")
}

export function toLine(event, starts) {
  try {
    if (!event || !types.includes(event.type)) return null
    const { data, type } = event
    if (!text(event.id) || !text(data.sessionID)) return null
    const usage = type === "session.usage.recorded"
    if (!usage && !text(data.assistantMessageID)) return null
    const key = `${data.sessionID}\0${data.assistantMessageID}`
    if (type === "session.step.started") {
      starts.delete(key)
      starts.set(key, {
        provider: optional(data.model?.providerID), model: optional(data.model?.id),
        variant: optional(data.model?.variant), agent: optional(data.agent),
      })
      if (starts.size > 10000) starts.delete(starts.keys().next().value)
      return null
    }
    const model = usage ? null : starts.get(key)
    if (!usage) starts.delete(key)
    if (!Number.isFinite(event.created) || !Number.isFinite(new Date(event.created).getTime())) return null
    if (usage && (typeof data.source !== "string" || !/^[a-z_]{1,32}$/.test(data.source))) return null
    const t = data.tokens
    const values = [t?.input, t?.output, t?.reasoning, t?.cache?.read, t?.cache?.write]
    if (!values.every((n) => Number.isInteger(n) && n >= 0)) return null
    return {
      v: 1, id: event.id, time: event.created, source: usage ? data.source : "step",
      status: usage ? "recorded" : type === "session.step.ended" ? "ended" : "failed",
      session: data.sessionID, message: usage ? null : data.assistantMessageID,
      provider: model?.provider ?? null, model: model?.model ?? null,
      variant: model?.variant ?? null, agent: model?.agent ?? null,
      tokens: { input: t.input, output: t.output, reasoning: t.reasoning,
        cacheRead: t.cache.read, cacheWrite: t.cache.write },
      cost: Number.isFinite(data.cost) && data.cost >= 0 ? data.cost : null,
    }
  } catch {
    return null
  }
}

// Every location instance in one OpenCode process receives every event. Instances
// and versions share only which events were already taken, so each is written once.
const shared = () => globalThis[Symbol.for("quota-hub.usage-bridge.v1")] ??= { seen: new Map(), starts: new Map() }

function claim(seen, event) {
  if (event?.type === "session.step.started" || !types.includes(event?.type) || typeof event.id !== "string") return true
  if (seen.has(event.id)) return false
  seen.set(event.id, true)
  if (seen.size > 10000) seen.delete(seen.keys().next().value)
  return true
}

export async function setupBridge(ctx) {
  const controller = new AbortController()
  const { seen, starts } = shared()
  let chain = Promise.resolve(), iterator, ready = false, lastError = -Infinity
  let stop
  const stopped = new Promise((resolve) => { stop = resolve })
  const report = () => {
    try {
      if (Date.now() - lastError < 60000) return
      lastError = Date.now()
      console.error("quota-hub.usage-bridge: запись событий недоступна")
    } catch {}
  }
  let pump = Promise.resolve()
  try {
    const directory = ctx.options?.directory
    const dir = typeof directory === "string" && isAbsolute(directory)
      ? directory : join(baseDir(), "events")
    const stream = ctx.event.subscribe({ types, signal: controller.signal })
    iterator = stream[Symbol.asyncIterator]()
    pump = (async () => {
      while (!controller.signal.aborted) {
        const next = await Promise.race([iterator.next(), stopped])
        if (!next || next.done || controller.signal.aborted) break
        if (!claim(seen, next.value)) continue
        const line = toLine(next.value, starts)
        if (!line) continue
        chain = chain.then(async () => {
          if (!ready) {
            await fs.mkdir(dir, { recursive: true, mode: 0o700 })
            ready = true
          }
          const day = new Date(line.time).toISOString().slice(0, 10)
          await fs.appendFile(join(dir, `${day}.jsonl`), `${JSON.stringify(line)}\n`, { mode: 0o600 })
        }).catch(() => { ready = false; report() })
      }
    })().catch(report)
  } catch {
    report()
  }
  return async () => {
    try {
      controller.abort()
      stop()
      try { Promise.resolve(iterator?.return?.()).catch(report) } catch { report() }
      let timer
      try {
        await Promise.race([
          pump.then(() => chain),
          new Promise((resolve) => { timer = setTimeout(resolve, 2000) }),
        ])
      } finally {
        clearTimeout(timer)
      }
    } catch { report() }
  }
}
