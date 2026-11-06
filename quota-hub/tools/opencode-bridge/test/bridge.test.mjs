import assert from "node:assert/strict"
import { chmod, lstat, mkdir, mkdtemp, readdir, readFile, readFile as realReadFile, rename, rm, stat, symlink, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"
import { after, test } from "node:test"
import {
  buildBridgeObservation,
  createBridge,
  DEFAULT_OUTPUT_FILE,
  PLUGIN_ID,
  REGISTRY_KEY,
  resolveConfig,
  setupBridge,
} from "../bridge.mjs"

await mkdir("/tmp/opencode", { recursive: true })
const roots = []
const freshFile = async (name = "claude.latest.json") => {
  const root = await mkdtemp(join("/tmp/opencode", "ob-test-"))
  roots.push(root)
  return join(root, "data", "bridge", name)
}
after(async () => {
  await Promise.all(roots.map((root) => rm(root, { recursive: true, force: true })))
})

const unhandled = []
process.on("unhandledRejection", (reason) => unhandled.push(reason))

const realFs = { chmod, lstat, mkdir, readFile: realReadFile, rename, rm, writeFile }
const URL_OK = "https://api.anthropic.com/v1/messages"
const read = async (file) => JSON.parse(await readFile(file, "utf8"))
const tick = () => new Promise((resolve) => setImmediate(resolve))
const until = async (predicate) => {
  for (let i = 0; i < 200 && !predicate(); i++) await new Promise((resolve) => setTimeout(resolve, 5))
  assert.ok(predicate(), "condition not reached")
}

const CLAUDE_HEADERS = {
  "anthropic-ratelimit-unified-5h-utilization": "0.38",
  "anthropic-ratelimit-unified-5h-reset": "1793800000",
  "anthropic-ratelimit-unified-5h-status": "allowed",
  "anthropic-ratelimit-unified-7d-utilization": "0.12",
  "anthropic-ratelimit-unified-7d-reset": "1794200000",
  "anthropic-ratelimit-unified-7d-status": "allowed",
  // everything below must never reach the file
  "anthropic-ratelimit-unified-status": "allowed",
  "anthropic-ratelimit-unified-representative-claim": "five_hour",
  "anthropic-ratelimit-unified-overage-utilization": "0.5",
  "anthropic-ratelimit-unified-fallback-percentage": "0.5",
  "anthropic-ratelimit-requests-limit": "4000",
  "retry-after": "30",
  authorization: "Bearer SECRET-TOKEN",
  "set-cookie": "sid=SECRET-COOKIE",
  "x-request-id": "SECRET-REQUEST",
}

const ALLOWED = {
  event: new Set(["model", "request", "response"]),
  model: new Set(["providerID"]),
  request: new Set(["url"]),
  response: new Set(["status", "headers"]),
}

/** Event whose every property read is recorded; forbidden ones also throw. */
function makeEvent({ provider = "anthropic", url = URL_OK, status = 200, headers = CLAUDE_HEADERS } = {}) {
  const touched = []
  const trap = (scope, target) =>
    new Proxy(target, {
      get(t, prop, receiver) {
        if (typeof prop === "symbol") return Reflect.get(t, prop, receiver)
        touched.push([scope, prop])
        if (!ALLOWED[scope].has(prop)) throw new Error(`forbidden read ${scope}.${prop}`)
        return Reflect.get(t, prop, receiver)
      },
      ownKeys() {
        touched.push([scope, "<ownKeys>"])
        throw new Error("forbidden enumeration")
      },
    })
  const boom = (name) => ({
    get() {
      touched.push(["raw", name])
      throw new Error(`forbidden ${name}`)
    },
  })
  const requestTarget = { url }
  for (const name of ["headers", "body", "bodyUsed", "clone", "text", "json", "arrayBuffer", "blob", "formData"]) {
    Object.defineProperty(requestTarget, name, boom(`request.${name}`))
  }
  const responseTarget = { status, headers: new Headers(headers) }
  for (const name of ["body", "bodyUsed", "clone", "text", "json", "arrayBuffer", "blob", "formData", "bytes"]) {
    Object.defineProperty(responseTarget, name, boom(`response.${name}`))
  }
  const eventTarget = {
    model: trap("model", { providerID: provider, id: "claude-secret-model" }),
    kind: "primary",
    request: trap("request", requestTarget),
    response: trap("response", responseTarget),
  }
  for (const name of ["sessionID", "agent", "messageID", "options", "prompt"]) {
    Object.defineProperty(eventTarget, name, boom(`event.${name}`))
  }
  return { event: trap("event", eventTarget), touched }
}

/** Isolated registry per test so the process-wide one is not shared by accident. */
const newBridge = (file, extra = {}) =>
  createBridge({ config: resolveConfig({ outputFile: file }), registry: new Map(), ...extra })

test("exact observation format, and the hook touches only allowed members", async () => {
  const file = await freshFile()
  const bridge = newBridge(file, { now: () => Date.UTC(2026, 10, 4, 12, 0, 0) })
  const { event, touched } = makeEvent()
  assert.equal(bridge.handle(event), undefined)
  await bridge.drain()

  assert.deepEqual(
    touched.filter(([scope, prop]) => scope === "raw" || !ALLOWED[scope]?.has(prop)),
    [],
  )
  assert.ok(touched.some(([scope, prop]) => scope === "response" && prop === "headers"))
  assert.ok(touched.some(([scope, prop]) => scope === "request" && prop === "url"))

  assert.deepEqual(await read(file), {
    schema_version: 1,
    source: "opencode-http-response-hook",
    observed_at: "2026-11-04T12:00:00.000Z",
    provider_id: "anthropic",
    endpoint: "api.anthropic.com/v1/messages",
    status: 200,
    quota: [
      { name: "anthropic-ratelimit-unified-5h-reset", type: "time", family: "subscription_candidate", value: "2026-11-04T13:46:40.000Z" },
      { name: "anthropic-ratelimit-unified-5h-status", type: "enum", family: "subscription_candidate", value: "allowed" },
      { name: "anthropic-ratelimit-unified-5h-utilization", type: "number", family: "subscription_candidate", value: 0.38 },
      { name: "anthropic-ratelimit-unified-7d-reset", type: "time", family: "subscription_candidate", value: "2026-11-09T04:53:20.000Z" },
      { name: "anthropic-ratelimit-unified-7d-status", type: "enum", family: "subscription_candidate", value: "allowed" },
      { name: "anthropic-ratelimit-unified-7d-utilization", type: "number", family: "subscription_candidate", value: 0.12 },
    ],
  })
  assert.equal(unhandled.length, 0)
})

test("no request info, PII, unrelated headers or header_names in the file", async () => {
  const file = await freshFile()
  const bridge = newBridge(file)
  bridge.handle(makeEvent().event)
  await bridge.drain()
  const text = await readFile(file, "utf8")
  for (const forbidden of ["SECRET", "Bearer", "authorization", "set-cookie", "x-request-id", "claude-secret-model", "retry-after", "overage", "representative", "requests-limit", "header_names", "model_id", "?"]) {
    assert.ok(!text.includes(forbidden), forbidden)
  }
  assert.deepEqual(Object.keys(await read(file)).sort(), ["endpoint", "observed_at", "provider_id", "quota", "schema_version", "source", "status"])
})

test("header values are fetched only for subscription-window headers", async () => {
  const gets = []
  const real = new Headers(CLAUDE_HEADERS)
  const spy = { keys: () => real.keys(), get: (n) => (gets.push(n), real.get(n)) }
  const observation = buildBridgeObservation({ url: URL_OK, status: 200, headers: spy, observedAt: 1_800_000_000_000 })
  assert.ok(observation)
  assert.equal(gets.length, 6)
  assert.ok(gets.every((n) => /^anthropic-ratelimit-unified-(5h|7d)-/.test(n)))
})

test("utilization ratio 0 is accepted", () => {
  const observation = buildBridgeObservation({
    url: URL_OK,
    status: 200,
    headers: new Headers({ "anthropic-ratelimit-unified-5h-utilization": "0", "anthropic-ratelimit-unified-5h-reset": "1793800000" }),
    observedAt: 1_800_000_000_000,
  })
  assert.deepEqual(
    observation.quota.map((q) => [q.name, q.value]),
    [
      ["anthropic-ratelimit-unified-5h-reset", "2026-11-04T13:46:40.000Z"],
      ["anthropic-ratelimit-unified-5h-utilization", 0],
    ],
  )
})

test("only 5h, 7d and 7d model windows are kept; overage and bare headers are not", () => {
  const headers = new Headers({
    "anthropic-ratelimit-unified-5h-utilization": "0.1",
    "anthropic-ratelimit-unified-7d-opus-utilization": "0.2",
    "anthropic-ratelimit-unified-7d-opus-surpassed-threshold": "0.75",
    "anthropic-ratelimit-unified-7d-sonnet-utilization": "0.3",
    "anthropic-ratelimit-unified-overage-utilization": "0.4",
    "anthropic-ratelimit-unified-overage-status": "allowed",
    "anthropic-ratelimit-unified-utilization": "0.9",
    "anthropic-ratelimit-unified-1d-utilization": "0.9",
  })
  const observation = buildBridgeObservation({ url: URL_OK, status: 200, headers, observedAt: 1_800_000_000_000 })
  assert.deepEqual(observation.quota.map((q) => q.name), [
    "anthropic-ratelimit-unified-5h-utilization",
    "anthropic-ratelimit-unified-7d-opus-surpassed-threshold",
    "anthropic-ratelimit-unified-7d-opus-utilization",
    "anthropic-ratelimit-unified-7d-sonnet-utilization",
  ])
})

test("a window without a valid utilization is dropped entirely", () => {
  const observation = buildBridgeObservation({
    url: URL_OK,
    status: 200,
    headers: new Headers({
      "anthropic-ratelimit-unified-5h-utilization": "abc",
      "anthropic-ratelimit-unified-5h-reset": "1793800000",
      "anthropic-ratelimit-unified-7d-utilization": "0.5",
    }),
    observedAt: 1_800_000_000_000,
  })
  assert.deepEqual(observation.quota.map((q) => q.name), ["anthropic-ratelimit-unified-7d-utilization"])
})

test("responses without a valid window utilization never overwrite the last good file", async () => {
  const file = await freshFile()
  let clock = 1_800_000_000_000
  const bridge = newBridge(file, { now: () => clock })
  bridge.handle(makeEvent().event)
  await bridge.drain()
  const before = await readFile(file, "utf8")

  const bad = [
    makeEvent({ headers: {} }).event,
    makeEvent({ headers: { "retry-after": "5", "anthropic-ratelimit-requests-limit": "10" } }).event,
    makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "NaN" } }).event,
    makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "-0.1" } }).event,
    makeEvent({ headers: { "anthropic-ratelimit-unified-5h-reset": "1793800000" } }).event,
    makeEvent({ status: 500 }).event,
    makeEvent({ status: 401 }).event,
    makeEvent({ provider: "openai" }).event,
    makeEvent({ url: "https://api.anthropic.com/v1/models" }).event,
    makeEvent({ url: "https://proxy.example/v1/messages" }).event,
    { model: { providerID: "anthropic" }, request: { url: URL_OK } },
    { model: { providerID: "anthropic" }, request: { url: URL_OK }, response: { status: 200, headers: { keys: () => 1 } } },
    null,
    undefined,
    "nope",
  ]
  for (const event of bad) {
    clock += 1000
    assert.doesNotThrow(() => bridge.handle(event))
  }
  await bridge.drain()
  assert.equal(await readFile(file, "utf8"), before)
  assert.equal(bridge.stats().observed, 1)
})

test("429 with window headers is persisted (rejected state matters)", async () => {
  const file = await freshFile()
  const bridge = newBridge(file)
  bridge.handle(makeEvent({ status: 429, headers: { "anthropic-ratelimit-unified-5h-utilization": "1", "anthropic-ratelimit-unified-5h-status": "rejected" } }).event)
  await bridge.drain()
  const saved = await read(file)
  assert.equal(saved.status, 429)
  assert.equal(saved.quota.find((q) => q.name.endsWith("5h-utilization")).value, 1)
})

test("handler is synchronous, writes later, and never reads the old file at startup", async () => {
  const file = await freshFile()
  await mkdir(dirname(file), { recursive: true })
  await writeFile(file, "{}\n")
  const reads = []
  let release
  const gate = new Promise((resolve) => (release = resolve))
  const fs = {
    ...realFs,
    readFile: (...args) => (reads.push(args[0]), realReadFile(...args)),
    writeFile: async (...args) => (await gate, realFs.writeFile(...args)),
  }
  const bridge = newBridge(file, { fs })
  assert.deepEqual(reads, [])
  assert.equal(bridge.handle(makeEvent().event), undefined)
  await tick()
  assert.equal(await readFile(file, "utf8"), "{}\n")
  release()
  await bridge.drain()
  assert.equal((await read(file)).provider_id, "anthropic")
})

test("queue collapses to the newest observation: one in flight plus one pending", async () => {
  const file = await freshFile()
  let release
  const gate = new Promise((resolve) => (release = resolve))
  let writes = 0
  const fs = { ...realFs, writeFile: async (...args) => (writes++, await gate, realFs.writeFile(...args)) }
  let clock = 1_800_000_000_000
  const bridge = newBridge(file, { fs, now: () => clock })
  for (let i = 0; i < 50; i++) {
    clock += 1000
    bridge.handle(makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": String(i / 100) } }).event)
  }
  await until(() => writes === 1)
  assert.equal(bridge.stats().pending, 1)
  release()
  await bridge.drain()
  assert.equal(writes, 2)
  assert.equal(bridge.stats().coalesced, 48)
  const saved = await read(file)
  assert.equal(saved.quota[0].value, 0.49)
  assert.equal(saved.observed_at, new Date(1_800_000_000_000 + 50_000).toISOString())
  assert.deepEqual((await readdir(dirname(file))).filter((n) => n.endsWith(".tmp")), [])
})

test("an older observation can never overwrite a newer one, within and across instances", async () => {
  const file = await freshFile()
  const registry = new Map()
  const clockA = { t: 1_800_000_100_000 }
  const clockB = { t: 1_800_000_000_000 }
  const config = resolveConfig({ outputFile: file })
  const a = createBridge({ config, registry, now: () => clockA.t })
  const b = createBridge({ config, registry, now: () => clockB.t })

  a.handle(makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "0.9" } }).event)
  b.handle(makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "0.1" } }).event)
  await Promise.all([a.drain(), b.drain()])
  assert.equal((await read(file)).quota[0].value, 0.9)
  assert.equal(b.stats().droppedOlder, 1)

  b.handle(makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "0.2" } }).event)
  await b.drain()
  assert.equal((await read(file)).quota[0].value, 0.9)

  clockB.t = 1_800_000_200_000
  b.handle(makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "0.3" } }).event)
  await b.drain()
  assert.equal((await read(file)).quota[0].value, 0.3)
})

test("instances share one writer per file through the process-wide registry", async () => {
  const file = await freshFile()
  const config = resolveConfig({ outputFile: file })
  const a = createBridge({ config })
  const b = createBridge({ config })
  try {
    assert.ok(globalThis[REGISTRY_KEY] instanceof Map)
    assert.equal(globalThis[REGISTRY_KEY].get(file).refs, 2)
    a.handle(makeEvent().event)
    b.handle(makeEvent().event)
    await Promise.all([a.drain(), b.drain()])
    assert.deepEqual(a.stats(), { ...b.stats() })
    assert.equal((await read(file)).provider_id, "anthropic")
  } finally {
    await a.dispose()
    await b.dispose()
  }
  assert.equal(globalThis[REGISTRY_KEY].has(file), false)
})

test("a newer file left on disk by another process is not overwritten", async () => {
  const file = await freshFile()
  await mkdir(dirname(file), { recursive: true, mode: 0o700 })
  const newer = { schema_version: 1, observed_at: "2030-01-01T00:00:00.000Z", marker: "other-process" }
  await writeFile(file, JSON.stringify(newer), { mode: 0o600 })
  const bridge = newBridge(file, { now: () => 1_800_000_000_000 })
  bridge.handle(makeEvent().event)
  await bridge.drain()
  assert.deepEqual(await read(file), newer)
  assert.equal(bridge.stats().skippedNewerOnDisk, 1)

  await writeFile(file, "not json at all")
  const later = newBridge(file, { now: () => 1_800_000_001_000 })
  later.handle(makeEvent().event)
  await later.drain()
  assert.equal((await read(file)).provider_id, "anthropic")
})

test("directory is 0700, file is 0600, no tmp is left", async () => {
  const file = await freshFile()
  await mkdir(dirname(file), { recursive: true, mode: 0o755 })
  await chmod(dirname(file), 0o755)
  const bridge = newBridge(file)
  bridge.handle(makeEvent().event)
  await bridge.drain()
  assert.equal((await stat(dirname(file))).mode & 0o777, 0o700)
  assert.equal((await stat(file)).mode & 0o777, 0o600)
  assert.deepEqual(await readdir(dirname(file)), ["claude.latest.json"])
})

test("a symlinked output directory is refused", async () => {
  const file = await freshFile()
  const root = dirname(dirname(file))
  const target = join(root, "real")
  await mkdir(target, { recursive: true })
  await rm(dirname(file), { recursive: true, force: true })
  await symlink(target, dirname(file))
  const bridge = newBridge(file)
  bridge.handle(makeEvent().event)
  await bridge.drain()
  assert.deepEqual(await readdir(target), [])
  assert.equal(bridge.stats().writeErrors, 1)
})

test("a symlink at the output path is replaced, not followed", async () => {
  const file = await freshFile()
  await mkdir(dirname(file), { recursive: true, mode: 0o700 })
  const victim = join(dirname(dirname(file)), "victim.txt")
  await writeFile(victim, "keep")
  await symlink(victim, file)
  const bridge = newBridge(file)
  bridge.handle(makeEvent().event)
  await bridge.drain()
  assert.equal(await readFile(victim, "utf8"), "keep")
  assert.equal((await lstat(file)).isFile(), true)
})

test("disk failures are swallowed, counted, and later writes recover", async () => {
  const file = await freshFile()
  let fail = "mkdir"
  const fs = {
    ...realFs,
    mkdir: async (...args) => {
      if (fail === "mkdir") throw new Error("EACCES")
      return realFs.mkdir(...args)
    },
    writeFile: (...args) => {
      if (fail === "write") throw new Error("ENOSPC")
      return realFs.writeFile(...args)
    },
  }
  let clock = 1_800_000_000_000
  const bridge = newBridge(file, { fs, now: () => (clock += 1000) })
  assert.doesNotThrow(() => bridge.handle(makeEvent().event))
  await bridge.drain()
  assert.equal(bridge.stats().writeErrors, 1)

  fail = "write"
  assert.doesNotThrow(() => bridge.handle(makeEvent().event))
  await bridge.drain()
  assert.equal(bridge.stats().writeErrors, 2)
  assert.deepEqual((await readdir(dirname(file))).filter((n) => n.endsWith(".tmp")), [])

  fail = null
  bridge.handle(makeEvent().event)
  await bridge.drain()
  assert.equal((await read(file)).provider_id, "anthropic")
  assert.equal(unhandled.length, 0)
})

test("hostile Headers implementations cannot break the handler", async () => {
  const bridge = newBridge(await freshFile())
  const evil = (headers) => ({ model: { providerID: "anthropic" }, request: { url: URL_OK }, response: { status: 200, headers } })
  assert.doesNotThrow(() => bridge.handle(evil({ keys() { throw new Error("boom") }, get() {} })))
  assert.doesNotThrow(() => bridge.handle(evil({ keys: () => ["anthropic-ratelimit-unified-5h-utilization"], get() { throw new Error("boom") } })))
  assert.doesNotThrow(() => bridge.handle({ get model() { throw new Error("boom") } }))
  await bridge.drain()
  assert.equal(unhandled.length, 0)
})

test("config: default path, valid path, and invalid paths disable the bridge", () => {
  assert.equal(resolveConfig(undefined).outputFile, DEFAULT_OUTPUT_FILE)
  assert.equal(resolveConfig({}).outputFile, DEFAULT_OUTPUT_FILE)
  assert.equal(resolveConfig({ outputFile: "/tmp/opencode/x/claude.json" }).outputFile, "/tmp/opencode/x/claude.json")
  for (const bad of ["relative/claude.json", "/tmp/../etc/x.json", "/tmp/dir/", "/", "", 5, "/tmp/a\0b", "/tmp//x.json"]) {
    assert.deepEqual(resolveConfig({ outputFile: bad }), { outputFile: null, error: "invalid outputFile" }, String(bad))
  }
})

function fakeCtx(options, { fail = false } = {}) {
  const hooks = []
  const ctx = {
    options,
    session: {
      async hook(name, callback, opts) {
        if (fail) throw new Error("registration refused")
        const registration = { name, callback, opts, disposed: 0, async dispose() { this.disposed++ } }
        hooks.push(registration)
        return registration
      },
    },
  }
  return { ctx, hooks }
}

test("setup registers one anthropic http.response hook and does no startup I/O", async () => {
  const file = await freshFile()
  const { ctx, hooks } = fakeCtx({ outputFile: file })
  const cleanup = await setupBridge(ctx, { registry: new Map() })
  assert.equal(PLUGIN_ID, "quota-hub.claude-bridge")
  assert.equal(hooks.length, 1)
  assert.equal(hooks[0].name, "http.response")
  assert.deepEqual(hooks[0].opts, { providerID: "anthropic" })
  assert.deepEqual(await readdir(dirname(dirname(dirname(file)))), [])
  await cleanup()
  assert.equal(hooks[0].disposed, 1)
  assert.deepEqual(await readdir(dirname(dirname(dirname(file)))), [])
})

test("cleanup waits for accepted writes, writes nothing stale, and stops observing", async () => {
  const file = await freshFile()
  const { ctx, hooks } = fakeCtx({ outputFile: file })
  let release
  const gate = new Promise((resolve) => (release = resolve))
  let writes = 0
  const fs = { ...realFs, writeFile: async (...args) => (writes++, await gate, realFs.writeFile(...args)) }
  const cleanup = await setupBridge(ctx, { registry: new Map(), fs })
  hooks[0].callback(makeEvent().event)
  await until(() => writes === 1)

  let done = false
  const cleaning = cleanup().then(() => (done = true))
  await tick()
  assert.equal(done, false)
  release()
  await cleaning
  assert.equal(done, true)

  const saved = await read(file)
  assert.equal(saved.provider_id, "anthropic")
  assert.deepEqual(await readdir(dirname(file)), ["claude.latest.json"])

  hooks[0].callback(makeEvent({ headers: { "anthropic-ratelimit-unified-5h-utilization": "0.99" } }).event)
  await tick()
  assert.deepEqual(await read(file), saved)
})

test("setup survives a refused registration, an invalid config and missing ctx pieces", async () => {
  const file = await freshFile()
  await assert.doesNotReject(async () => (await setupBridge(fakeCtx({ outputFile: file }, { fail: true }).ctx, { registry: new Map() }))())
  const invalid = fakeCtx({ outputFile: "relative.json" })
  await assert.doesNotReject(async () => (await setupBridge(invalid.ctx, { registry: new Map() }))())
  assert.equal(invalid.hooks.length, 0)
  await assert.doesNotReject(async () => (await setupBridge({ options: { outputFile: file }, session: {} }, { registry: new Map() }))())
  await assert.doesNotReject(async () => (await setupBridge(undefined, { registry: new Map() }))())
  assert.equal(unhandled.length, 0)
})

test("server.ts is a thin Plugin.define wrapper and the sources use no forbidden APIs", async () => {
  const server = await readFile(new URL("../server.ts", import.meta.url), "utf8")
  assert.match(server, /import \{ Plugin \} from "@opencode\/plugin"/)
  assert.match(server, /Plugin\.define\(\{\s*id: PLUGIN_ID,\s*async setup\(ctx\)/)
  const bridge = await readFile(new URL("../bridge.mjs", import.meta.url), "utf8")
  const code = bridge.replace(/^\s*\/\/.*$/gm, "")
  for (const pattern of [/\bfetch\s*\(/, /console\./, /setTimeout|setInterval/, /\.clone\s*\(/, /\.text\s*\(/, /\.json\s*\(/, /\bnode:(http|https|net|child_process)\b/, /\bfsync\b/i, /\.credential|integration/i]) {
    assert.doesNotMatch(code, pattern, String(pattern))
  }
})

test("runtime imports stay inside the plugin for OpenCode symlink loading", async () => {
  for (const filename of ["server.ts", "bridge.mjs", "headers.mjs"]) {
    const source = await readFile(new URL(`../${filename}`, import.meta.url), "utf8")
    for (const match of source.matchAll(/from\s+["']([^"']+)["']/g)) {
      const specifier = match[1]
      if (!specifier.startsWith(".")) continue
      assert.ok(specifier.startsWith("./"), `external relative import breaks symlink discovery: ${specifier}`)
      await readFile(new URL(`../${specifier.slice(2)}`, import.meta.url), "utf8")
    }
  }
})
