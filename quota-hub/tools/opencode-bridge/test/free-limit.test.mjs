import assert from "node:assert/strict"
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises"
import { join } from "node:path"
import { after, test } from "node:test"
import { buildFreeLimitObservation, matchInferenceEndpoint, readRetryAfter } from "../free-limit.mjs"
import { createFreeLimitBridge, resolveConfig } from "../bridge.mjs"

await mkdir("/tmp/opencode", { recursive: true })
const roots = []
const freshFile = async (name = "opencode.latest.json") => {
  const root = await mkdtemp(join("/tmp/opencode", "ofl-test-"))
  roots.push(root)
  return join(root, "data", "bridge", name)
}
after(async () => {
  await Promise.all(roots.map((root) => rm(root, { recursive: true, force: true })))
})

const NOW = Date.parse("2026-11-10T12:00:00.000Z")
const headers = (values) => ({ get: (name) => values[name.toLowerCase()] ?? null })

test("only the Console inference endpoint is observed", () => {
  assert.equal(matchInferenceEndpoint("https://opencode.ai/inference/openai/v1/chat/completions"), "opencode.ai/inference")
  assert.equal(matchInferenceEndpoint("https://opencode.ai/inference/go/openai/v1/responses"), "opencode.ai/inference")
  assert.equal(matchInferenceEndpoint("https://opencode.ai/zen/go/v1/usage"), null)
  assert.equal(matchInferenceEndpoint("https://api.anthropic.com/v1/messages"), null)
  assert.equal(matchInferenceEndpoint("http://opencode.ai/inference/openai/v1/chat/completions"), null)
  assert.equal(matchInferenceEndpoint("https://opencode.ai.evil.test/inference/x"), null)
  assert.equal(matchInferenceEndpoint("not a url"), null)
  assert.equal(matchInferenceEndpoint(undefined), null)
})

test("Retry-After is read as seconds or a date and stays bounded", () => {
  assert.equal(readRetryAfter(headers({ "retry-after": "43185" }), NOW), NOW + 43185 * 1000)
  assert.equal(readRetryAfter(headers({ "retry-after": "Wed, 11 Nov 2026 00:00:00 GMT" }), NOW), Date.parse("2026-11-11T00:00:00.000Z"))
  assert.equal(readRetryAfter(headers({ "retry-after": "0" }), NOW), null)
  assert.equal(readRetryAfter(headers({ "retry-after": "-5" }), NOW), null)
  assert.equal(readRetryAfter(headers({ "retry-after": "999999999" }), NOW), null)
  assert.equal(readRetryAfter(headers({}), NOW), null)
  assert.equal(readRetryAfter(null, NOW), null)
})

test("a refused free model yields exactly the reset moment", () => {
  const observation = buildFreeLimitObservation({
    url: "https://opencode.ai/inference/openai/v1/chat/completions",
    status: 429,
    headers: headers({ "retry-after": "43185", "x-request-id": "SECRET-REQUEST", authorization: "Bearer SECRET" }),
    observedAt: NOW,
    now: () => NOW,
  })
  assert.equal(observation.schema_version, 1)
  assert.equal(observation.provider_id, "opencode")
  assert.equal(observation.source, "opencode-http-response-hook")
  assert.equal(observation.status, 429)
  assert.equal(observation.observed_at, new Date(NOW).toISOString())
  assert.equal(observation.quota.length, 1)
  assert.equal(observation.quota[0].name, "free_limit_reset")
  assert.equal(observation.quota[0].type, "time")
  assert.equal(observation.quota[0].value, new Date(NOW + 43185 * 1000).toISOString())
  const raw = JSON.stringify(observation)
  for (const secret of ["SECRET-REQUEST", "SECRET"]) {
    assert.ok(!raw.includes(secret), `secret ${secret} leaked`)
  }
})

test("a successful response or a foreign endpoint yields nothing", () => {
  const common = { headers: headers({ "retry-after": "60" }), observedAt: NOW, now: () => NOW }
  assert.equal(buildFreeLimitObservation({ ...common, url: "https://opencode.ai/inference/x", status: 200 }), null)
  assert.equal(buildFreeLimitObservation({ ...common, url: "https://api.openai.com/v1/chat", status: 429 }), null)
  assert.equal(buildFreeLimitObservation({ ...common, url: "https://opencode.ai/inference/x", status: 429, headers: headers({}) }), null)
})

test("the bridge writes the refusal to its own file and nothing else", async () => {
  const file = await freshFile()
  const { createFreeLimitBridge: create } = { createFreeLimitBridge }
  const bridge = create({ config: resolveConfig({ freeLimitFile: file }), registry: new Map(), now: () => NOW })
  const event = {
    model: { providerID: "opencode" },
    request: { url: "https://opencode.ai/inference/openai/v1/chat/completions" },
    response: { status: 429, headers: headers({ "retry-after": "60" }) },
  }
  bridge.handle(event)
  await bridge.drain()
  const saved = JSON.parse(await readFile(file, "utf8"))
  assert.equal(saved.provider_id, "opencode")
  assert.equal(saved.quota[0].name, "free_limit_reset")
  // a foreign provider never reaches the file
  bridge.handle({ ...event, model: { providerID: "anthropic" } })
  await bridge.drain()
  assert.equal(JSON.parse(await readFile(file, "utf8")).observed_at, saved.observed_at)
  await bridge.dispose()
})
