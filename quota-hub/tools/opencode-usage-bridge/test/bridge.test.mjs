import test from "node:test"
import assert from "node:assert/strict"
import * as fs from "node:fs/promises"
import os from "node:os"
import { join } from "node:path"
import { setImmediate as tick } from "node:timers/promises"
import { PLUGIN_ID, baseDir, setupBridge, toLine } from "../bridge.mjs"

const fixture = JSON.parse(await fs.readFile(new URL("./fixtures/runtime-v2.3-contract.json", import.meta.url)))
const [start, end, failed, title, compaction, generate] = fixture.events
const tokens = { input: 10, output: 4, reasoning: 2, cacheRead: 3, cacheWrite: 2 }
const event = (id, time = 1010) => ({ ...end, id, created: time })

async function temporary(t) {
  const dir = await fs.mkdtemp(join(os.tmpdir(), "usage-bridge-test-"))
  t.after(() => fs.rm(dir, { recursive: true, force: true }))
  return dir
}

async function until(check) {
  const deadline = Date.now() + 1000
  while (!await check()) {
    assert.ok(Date.now() < deadline, "condition was not reached")
    await tick()
  }
}

function channel() {
  const pending = [], values = []
  let subscription, returns = 0
  const iterator = {
    next() {
      if (values.length) return Promise.resolve({ value: values.shift(), done: false })
      return new Promise((resolve) => pending.push(resolve))
    },
    return() { returns++; return new Promise(() => {}) },
    [Symbol.asyncIterator]() { return this },
  }
  return {
    event: { subscribe(options) { subscription = options; return iterator } },
    push(value) {
      if (pending.length) pending.shift()({ value, done: false })
      else values.push(value)
    },
    get subscription() { return subscription },
    get returns() { return returns },
  }
}

async function readLines(dir) {
  const names = await fs.readdir(dir)
  const content = await Promise.all(names.filter((n) => n.endsWith(".jsonl"))
    .sort().map((n) => fs.readFile(join(dir, n), "utf8")))
  return content.flatMap((body) => {
    assert.ok(body.endsWith("\n"))
    return body.trimEnd().split("\n").map((line) => JSON.parse(line))
  })
}

test("baseDir follows absolute XDG_STATE_HOME only", () => {
  assert.equal(PLUGIN_ID, "quota-hub.usage-bridge")
  assert.equal(baseDir({ XDG_STATE_HOME: "/state" }, "/srv/example"), "/state/quota-usage")
  assert.equal(baseDir({ XDG_STATE_HOME: "state" }, "/srv/example"), "/srv/example/.local/state/quota-usage")
  assert.equal(baseDir({}, "/srv/example"), "/srv/example/.local/state/quota-usage")
  assert.equal(baseDir({ XDG_STATE_HOME: "" }, "/srv/example"), "/srv/example/.local/state/quota-usage")
})

test("v2.3 fixture: start pairs with end; usage has no model; failed without tokens is skipped", () => {
  const starts = new Map()
  const lines = fixture.events.map((e) => toLine(e, starts)).filter(Boolean)
  assert.equal(lines.length, 4)
  assert.deepEqual(lines[0], {
    v: 1, id: "event-end", time: 1010, source: "step", status: "ended",
    session: "session-test", message: "message-test",
    provider: "synthetic", model: "offline-model", variant: "medium", agent: "build",
    tokens, cost: 0.0000233,
  })
  assert.equal(starts.size, 0)
  for (const [index, source] of ["title", "compaction", "generate"].entries()) {
    assert.deepEqual(lines[index + 1], {
      v: 1, id: `event-${source}`, time: 1030 + index * 10, source,
      status: "recorded", session: "session-test", message: null,
      provider: null, model: null, variant: null, agent: null,
      tokens: { input: 10, output: 4, reasoning: 0, cacheRead: 0, cacheWrite: 0 }, cost: 0.001,
    })
  }
})

test("unpaired end and failed retain tokens, not guessed metadata", () => {
  const line = toLine(end, new Map())
  for (const field of ["provider", "model", "variant", "agent"]) assert.equal(line[field], null)
  const starts = new Map()
  toLine({ ...start, data: { ...start.data, assistantMessageID: "message-failed" } }, starts)
  const result = toLine({ ...failed, data: { ...failed.data, tokens: end.data.tokens } }, starts)
  assert.equal(result.status, "failed")
  assert.equal(result.cost, null)
  assert.equal(result.model, "offline-model")
  assert.deepEqual(result.tokens, tokens)
  assert.equal(starts.size, 0)
  toLine(start, starts)
  assert.equal(toLine({ ...failed, data: { ...failed.data, assistantMessageID: start.data.assistantMessageID } }, starts), null)
  assert.equal(starts.size, 0)
})

test("pair key includes session and message; oldest starts are evicted at 10000", () => {
  const starts = new Map()
  toLine(start, starts)
  assert.equal(toLine({ ...end, data: { ...end.data, sessionID: "other" } }, starts).model, null)
  assert.equal(starts.size, 1)
  for (let i = 0; i < 10000; i++) {
    toLine({ ...start, data: { ...start.data, assistantMessageID: `message-${i}` } }, starts)
  }
  assert.equal(starts.size, 10000)
  assert.equal(starts.has("session-test\0message-test"), false)
  assert.equal(starts.has("session-test\0message-9999"), true)
})

test("malformed events and unrelated types never throw or produce lines", () => {
  const bad = [null, undefined, 1, "", [], {}, { type: "http.response" },
    { ...end, data: null }, { ...end, id: "" }, { ...end, id: "x".repeat(257) },
    { ...end, created: NaN }, { ...end, created: Infinity }, { ...end, created: 1e20 },
    { ...end, created: "1010" }]
  for (const field of ["sessionID", "assistantMessageID"]) {
    for (const value of [null, "", 1, "x".repeat(257)]) {
      bad.push({ ...end, data: { ...end.data, [field]: value } })
    }
  }
  for (const value of [-1, 1.1, NaN, Infinity, "1", undefined]) {
    for (const field of ["input", "output", "reasoning"]) {
      bad.push({ ...end, data: { ...end.data, tokens: { ...end.data.tokens, [field]: value } } })
    }
    for (const field of ["read", "write"]) {
      bad.push({ ...end, data: { ...end.data, tokens: { ...end.data.tokens,
        cache: { ...end.data.tokens.cache, [field]: value } } } })
    }
  }
  for (const source of ["", "Title", "a-b", "x".repeat(33), null, 1]) {
    bad.push({ ...title, data: { ...title.data, source } })
  }
  bad.push({ get type() { throw new Error("bad getter") } })
  for (const e of bad) assert.equal(toLine(e, new Map()), null)
})

test("invalid cost becomes null; zero tokens and zero cost remain valid", () => {
  for (const cost of [undefined, NaN, Infinity, -1, "0"]) {
    assert.equal(toLine({ ...end, data: { ...end.data, cost } }, new Map()).cost, null)
  }
  const zero = toLine({ ...end, data: { ...end.data, cost: 0,
    tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } } }, new Map())
  assert.equal(zero.cost, 0)
  assert.deepEqual(Object.values(zero.tokens), [0, 0, 0, 0, 0])
})

test("two independent instances append 500 events each; modes, filtering and UTC dates", async (t) => {
  const root = await temporary(t)
  const dir = join(root, "events")
  const a = channel(), b = channel()
  const ca = await setupBridge({ options: { directory: dir, enabled: false, homeState: "/ignored" }, event: a.event })
  const cb = await setupBridge({ options: { directory: dir }, event: b.event })
  t.after(async () => { await ca(); await cb() })
  assert.deepEqual(a.subscription.types, [
    "session.step.started", "session.step.ended", "session.step.failed", "session.usage.recorded",
  ])
  a.push({ ...end, type: "unrelated" })
  a.push(null)
  a.push(start)
  for (let i = 0; i < 500; i++) {
    a.push(event(`a-${i}`))
    b.push(event(`b-${i}`, Date.parse("2026-11-18T00:00:00Z")))
  }
  await until(async () => {
    try { return (await readLines(dir)).length === 1000 } catch { return false }
  })
  await ca()
  await cb()
  const lines = await readLines(dir)
  assert.equal(lines.length, 1000)
  assert.equal(new Set(lines.map((l) => l.id)).size, 1000)
  assert.equal(lines.find((l) => l.id === "a-0").model, "offline-model")
  assert.equal(a.subscription.signal.aborted, true)
  assert.equal(b.subscription.signal.aborted, true)
  assert.deepEqual((await fs.readdir(dir)).sort(), ["1970-01-01.jsonl", "2026-11-18.jsonl"])
  assert.equal((await fs.stat(dir)).mode & 0o777, 0o700)
  assert.equal((await fs.stat(join(dir, "1970-01-01.jsonl"))).mode & 0o777, 0o600)
})

test("cleanup never awaits a stuck next() or return()", async (t) => {
  const dir = await temporary(t)
  const source = channel()
  const cleanup = await setupBridge({ options: { directory: dir }, event: source.event })
  const before = Date.now()
  await cleanup()
  assert.ok(Date.now() - before < 500)
  assert.equal(source.subscription.signal.aborted, true)
  assert.equal(source.returns, 1)
})

test("cleanup flushes already queued writes", async (t) => {
  const dir = await temporary(t)
  let processed
  const consumed = new Promise((resolve) => { processed = resolve })
  const source = {
    async *subscribe() {
      for (let i = 0; i < 50; i++) yield event(`flush-${i}`)
      processed()
    },
  }
  const cleanup = await setupBridge({ options: { directory: dir }, event: source })
  await consumed
  await cleanup()
  assert.equal((await readLines(dir)).length, 50)
})

test("relative directory is ignored in favor of the default events directory", async (t) => {
  const root = await temporary(t)
  const old = process.env.XDG_STATE_HOME
  process.env.XDG_STATE_HOME = root
  t.after(() => {
    if (old === undefined) delete process.env.XDG_STATE_HOME
    else process.env.XDG_STATE_HOME = old
  })
  const source = { async *subscribe() { yield title; yield compaction; yield generate } }
  const cleanup = await setupBridge({
    options: { directory: "relative", homeState: "/ignored", enabled: false }, event: source,
  })
  const dir = join(root, "quota-usage", "events")
  await until(async () => {
    try { return (await readLines(dir)).length === 3 } catch { return false }
  })
  await cleanup()
  assert.deepEqual((await readLines(dir)).map((l) => l.source), ["title", "compaction", "generate"])
})

test("write errors are contained and rate-limited; subsequent writes recover", async (t) => {
  const root = await temporary(t)
  const dir = join(root, "events")
  await fs.writeFile(dir, "not a directory")
  const logs = []
  t.mock.method(console, "error", (...args) => { logs.push(args) })
  const source = channel()
  const cleanup = await setupBridge({ options: { directory: dir }, event: source.event })
  t.after(cleanup)
  source.push(event("bad-1"))
  await until(() => logs.length === 1)
  source.push(event("bad-2"))
  source.push(event("bad-3"))
  await tick()
  await fs.unlink(dir)
  source.push(event("recovered"))
  await until(async () => {
    try { return (await readLines(dir)).some((l) => l.id === "recovered") } catch { return false }
  })
  await cleanup()
  assert.equal(logs.length, 1)
})

test("subscription failure, iterator failure and throwing logger do not escape", async (t) => {
  const dir = await temporary(t)
  t.mock.method(console, "error", () => { throw new Error("logger failed") })
  for (const subscribe of [
    () => { throw new Error("subscribe failed") },
    () => ({ [Symbol.asyncIterator]() { return this }, next() { throw new Error("next failed") } }),
    () => ({ [Symbol.asyncIterator]() { return this }, next() { return Promise.reject(new Error("next rejected")) } }),
  ]) {
    const cleanup = await setupBridge({ options: { directory: dir }, event: { subscribe } })
    await cleanup()
  }
})
