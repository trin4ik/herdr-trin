import test from "node:test"
import assert from "node:assert/strict"
import * as fs from "node:fs/promises"
import os from "node:os"
import { join } from "node:path"
import { setImmediate as tick } from "node:timers/promises"
import { rateLimitOf, setupBridge } from "../bridge.mjs"
import { claudeProviders, claudeQuotaFile, quotaFileOption, toObservation, writeObservation } from "../ratelimit.mjs"

test.beforeEach(() => { delete globalThis[Symbol.for("quota-hub.usage-bridge.v1")] })

// Synthetic rate_limit_info in the shape Claude Code emits as rate_limit_event.
const info = {
  status: "allowed", resetsAt: 1795468200, rateLimitType: "five_hour", overageStatus: "rejected",
  unifiedWindows: {
    five_hour: { utilization: 0.02, resetsAt: 1795468200, status: "allowed" },
    seven_day: { utilization: 0, resetsAt: 1795485600, status: "allowed" },
    seven_day_opus: { utilization: 0.4, resetsAt: 1795485600 },
  },
}
const created = Date.parse("2026-11-23T16:36:17.297Z")
const start = (provider, id = "msg-1") => ({
  id: `start-${id}`, created, type: "session.step.started",
  data: { sessionID: "ses-1", assistantMessageID: id, agent: "build", model: { providerID: provider, id: "opus" } },
})
const ended = (rateLimit, id = "msg-1", time = created) => ({
  id: `end-${id}`, created: time, type: "session.step.ended",
  data: { sessionID: "ses-1", assistantMessageID: id, finish: "stop", native: { rateLimit },
    tokens: { input: 1, output: 1, reasoning: 0, cache: { read: 0, write: 0 } } },
})

test("unified windows map to the bridge schema; zero stays a value", () => {
  assert.deepEqual(toObservation(info, created), {
    schema_version: 1, source: "opencode-claude-agent", provider_id: "anthropic",
    observed_at: "2026-11-23T16:36:17.297Z", status: 200,
    quota: [
      { name: "anthropic-ratelimit-unified-5h-utilization", type: "number", value: 0.02 },
      { name: "anthropic-ratelimit-unified-5h-reset", type: "time", value: "2026-11-23T21:10:00.000Z" },
      { name: "anthropic-ratelimit-unified-7d-utilization", type: "number", value: 0 },
      { name: "anthropic-ratelimit-unified-7d-reset", type: "time", value: "2026-11-24T02:00:00.000Z" },
      { name: "anthropic-ratelimit-unified-7d_opus-utilization", type: "number", value: 0.4 },
      { name: "anthropic-ratelimit-unified-7d_opus-reset", type: "time", value: "2026-11-24T02:00:00.000Z" },
    ],
  })
})

test("primary window without unifiedWindows; rejected becomes 429", () => {
  const o = toObservation({ status: "rejected", rateLimitType: "five_hour", utilization: 1, resetsAt: 1795468200 }, created)
  assert.equal(o.status, 429)
  assert.deepEqual(o.quota.map((q) => q.name), [
    "anthropic-ratelimit-unified-5h-utilization", "anthropic-ratelimit-unified-5h-reset",
  ])
})

test("missing, malformed or unknown windows produce nothing; no value is invented", () => {
  for (const bad of [undefined, null, 1, "x", [], {}, { unifiedWindows: [] },
    { rateLimitType: "five_hour" }, { rateLimitType: "five_hour", utilization: -1 },
    { rateLimitType: "five_hour", utilization: NaN }, { rateLimitType: "five_hour", utilization: "0.1" },
    { unifiedWindows: { overage: { utilization: 0.1 } } }, { unifiedWindows: { "five_hour-x": { utilization: 0.1 } } }]) {
    assert.equal(toObservation(bad, created), null)
  }
  assert.equal(toObservation(info, NaN), null)
  const noReset = toObservation({ unifiedWindows: { five_hour: { utilization: 0.1, resetsAt: "soon" } } }, created)
  assert.deepEqual(noReset.quota.map((q) => q.name), ["anthropic-ratelimit-unified-5h-utilization"])
})

test("only finished steps of Claude providers are taken", () => {
  const providers = claudeProviders(undefined)
  assert.deepEqual(providers, ["claude-agent"])
  const starts = new Map()
  const key = "ses-1\0msg-1"
  starts.set(key, { provider: "claude-agent" })
  assert.equal(rateLimitOf(ended(info), starts, providers).quota[0].value, 0.02)
  assert.equal(rateLimitOf({ ...ended(info), type: "session.step.failed" }, starts, providers).status, 200)
  assert.equal(rateLimitOf({ ...ended(info), type: "session.step.started" }, starts, providers), null)
  assert.equal(rateLimitOf(ended(undefined), starts, providers), null)
  starts.set(key, { provider: "openai" })
  assert.equal(rateLimitOf(ended(info), starts, providers), null)
  assert.equal(rateLimitOf(ended(info), new Map(), providers), null)
  assert.equal(rateLimitOf(null, starts, providers), null)
  assert.deepEqual(claudeProviders(["other"]), ["other"])
  assert.deepEqual(claudeProviders([""]), ["claude-agent"])
})

test("file option must be absolute; default is the quota-hub bridge path", () => {
  assert.equal(claudeQuotaFile("/srv/example"), "/srv/example/.local/state/quota-hub/bridge/claude.latest.json")
  assert.equal(quotaFileOption("/x/claude.json"), "/x/claude.json")
  assert.equal(quotaFileOption("relative.json"), claudeQuotaFile())
})

test("writes atomically with private modes; older never replaces newer", async (t) => {
  const root = await fs.mkdtemp(join(os.tmpdir(), "usage-bridge-limits-"))
  t.after(() => fs.rm(root, { recursive: true, force: true }))
  const file = join(root, "bridge", "claude.latest.json")
  const state = {}
  assert.equal(await writeObservation(file, toObservation(info, created), state), true)
  assert.equal(await writeObservation(file, toObservation({ rateLimitType: "five_hour", utilization: 0.9 }, created - 1000), state), false)
  const saved = JSON.parse(await fs.readFile(file, "utf8"))
  assert.equal(saved.quota[0].value, 0.02)
  assert.equal((await fs.stat(join(root, "bridge"))).mode & 0o777, 0o700)
  assert.equal((await fs.stat(file)).mode & 0o777, 0o600)
  assert.deepEqual(await fs.readdir(join(root, "bridge")), ["claude.latest.json"])
})

test("plugin writes the latest Claude answer limits next to usage lines", async (t) => {
  const root = await fs.mkdtemp(join(os.tmpdir(), "usage-bridge-plugin-"))
  t.after(() => fs.rm(root, { recursive: true, force: true }))
  const file = join(root, "bridge", "claude.latest.json")
  const events = [start("claude-agent"), ended(info),
    start("openai", "msg-2"), ended({ ...info, unifiedWindows: { five_hour: { utilization: 0.99 } } }, "msg-2", created + 1000),
    start("claude-agent", "msg-3"), ended({ unifiedWindows: { five_hour: { utilization: 0.03, resetsAt: 1795468200 } } }, "msg-3", created + 2000)]
  const source = { async *subscribe() { yield* events } }
  const cleanup = await setupBridge({ options: { directory: join(root, "events"), claudeQuotaFile: file }, event: source })
  const deadline = Date.now() + 1000
  for (;;) {
    const saved = await fs.readFile(file, "utf8").then(JSON.parse, () => null)
    if (saved?.quota[0].value === 0.03) break
    assert.ok(Date.now() < deadline, "limits were not written")
    await tick()
  }
  await cleanup()
  const saved = JSON.parse(await fs.readFile(file, "utf8"))
  assert.equal(saved.observed_at, new Date(created + 2000).toISOString())
  const lines = (await fs.readFile(join(root, "events", "2026-11-23.jsonl"), "utf8")).trimEnd().split("\n")
  assert.equal(lines.length, 3)
})
