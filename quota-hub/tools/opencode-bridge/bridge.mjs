// Persistent Claude-only bridge: OpenCode http.response -> latest sanitized observation file.
// Passive by design: it reads only model.providerID, request.url (filter, never stored),
// response.status and the names/values of the Anthropic subscription-window headers.
import { chmod, lstat, mkdir, readFile, rename, rm, writeFile } from "node:fs/promises"
import { basename, dirname, isAbsolute, join, resolve } from "node:path"
import { homedir } from "node:os"
import { buildObservation, matchEndpoint, SCHEMA_VERSION } from "./headers.mjs"

export const PLUGIN_ID = "quota-hub.claude-bridge"
export const PROVIDER_ID = "anthropic"
export const SOURCE = "opencode-http-response-hook"
export const DEFAULT_OUTPUT_FILE = join(homedir(), ".local", "state", "quota-hub", "bridge", "claude.latest.json")
export const REGISTRY_KEY = Symbol.for("quota-hub.claude-bridge.registry.v1")

const MAX_HEADER_NAMES_SCANNED = 256
const MAX_EXISTING_FILE_BYTES = 64 * 1024

const WINDOW_HEADER_RE =
  /^anthropic-ratelimit-unified-(5h|7d|7d-opus|7d-sonnet)-(utilization|reset|status|surpassed-threshold)$/
const UTILIZATION_SUFFIX = "-utilization"

const defaultFs = { chmod, lstat, mkdir, readFile, rename, rm, writeFile }

/** An explicitly given but unusable outputFile disables the bridge instead of writing elsewhere. */
export function resolveConfig(options) {
  const source = options !== null && typeof options === "object" ? options : {}
  const given = source.outputFile
  if (given === undefined || given === null) return { outputFile: DEFAULT_OUTPUT_FILE, error: null }
  const valid =
    typeof given === "string" &&
    !given.includes("\0") &&
    isAbsolute(given) &&
    resolve(given) === given &&
    dirname(given) !== given &&
    basename(given) !== ""
  return valid ? { outputFile: given, error: null } : { outputFile: null, error: "invalid outputFile" }
}

const acceptableStatus = (status) => (status >= 200 && status <= 299) || status === 429

/** Copies only subscription-window headers (never anything else) out of a Headers-like object. */
function pickWindowHeaders(headers) {
  if (headers === null || typeof headers !== "object") return null
  if (typeof headers.keys !== "function" || typeof headers.get !== "function") return null
  const picked = new Map()
  let scanned = 0
  for (const rawName of headers.keys()) {
    if (scanned++ >= MAX_HEADER_NAMES_SCANNED) break
    if (typeof rawName !== "string") continue
    const name = rawName.toLowerCase()
    if (WINDOW_HEADER_RE.test(name) && !picked.has(name)) picked.set(name, headers.get(name))
  }
  return picked
}

/**
 * Returns the only object that is ever persisted, or null when the response carries no
 * usable window utilization (then nothing may replace the last good observation).
 */
export function buildBridgeObservation({ url, status, headers, observedAt }) {
  const endpoint = matchEndpoint(PROVIDER_ID, url)
  if (endpoint === null) return null
  if (!Number.isInteger(status) || !acceptableStatus(status)) return null
  const picked = pickWindowHeaders(headers)
  if (picked === null || picked.size === 0) return null

  const built = buildObservation({ providerID: PROVIDER_ID, status, endpoint, headers: picked, observedAt })

  const windows = new Map()
  for (const entry of built.quota) {
    const match = WINDOW_HEADER_RE.exec(entry.name)
    if (!match) continue
    if (!windows.has(match[1])) windows.set(match[1], [])
    windows.get(match[1]).push(entry)
  }

  const quota = []
  for (const entries of windows.values()) {
    const hasUtilization = entries.some(
      (entry) =>
        entry.name.endsWith(UTILIZATION_SUFFIX) &&
        entry.type === "number" &&
        typeof entry.value === "number" &&
        Number.isFinite(entry.value) &&
        entry.value >= 0,
    )
    if (hasUtilization) quota.push(...entries)
  }
  if (quota.length === 0) return null
  quota.sort((a, b) => (a.name < b.name ? -1 : 1))

  return {
    schema_version: SCHEMA_VERSION,
    source: SOURCE,
    observed_at: built.observed_at,
    provider_id: PROVIDER_ID,
    endpoint,
    status: built.status,
    quota: quota.map(({ name, type, family, value }) => ({ name, type, family, value })),
  }
}

const getRegistry = () => (globalThis[REGISTRY_KEY] ??= new Map())

/**
 * One writer per output file and process. At most one write in flight plus one pending
 * (the newest), so memory and disk traffic stay bounded however busy the model traffic is.
 */
function createStream(path, fs) {
  const stream = {
    path,
    fs,
    refs: 0,
    pending: null,
    runner: null,
    newestMs: -Infinity,
    dirReady: null,
    tmpSeq: 0,
    stats: { written: 0, coalesced: 0, droppedOlder: 0, skippedNewerOnDisk: 0, writeErrors: 0 },
  }

  const ensureDir = () => {
    stream.dirReady ??= (async () => {
      const dir = dirname(path)
      await fs.mkdir(dir, { recursive: true, mode: 0o700 })
      const info = await fs.lstat(dir)
      if (info.isSymbolicLink() || !info.isDirectory()) throw new Error("output directory is not a plain directory")
      if (typeof process.getuid === "function" && info.uid !== process.getuid()) throw new Error("output directory is not ours")
      if ((info.mode & 0o077) !== 0) await fs.chmod(dir, 0o700)
    })().catch((error) => {
      stream.dirReady = null
      throw error
    })
    return stream.dirReady
  }

  const existingObservedMs = async () => {
    try {
      const info = await fs.lstat(path)
      if (!info.isFile() || info.size > MAX_EXISTING_FILE_BYTES) return null
      const ms = Date.parse(JSON.parse(await fs.readFile(path, "utf8"))?.observed_at)
      return Number.isFinite(ms) ? ms : null
    } catch {
      return null
    }
  }

  const write = async ({ ms, observation }) => {
    await ensureDir()
    const onDisk = await existingObservedMs()
    if (onDisk !== null && onDisk > ms) {
      stream.stats.skippedNewerOnDisk++
      return
    }
    const tmp = join(dirname(path), `.${basename(path)}.${process.pid}.${++stream.tmpSeq}.tmp`)
    try {
      await fs.writeFile(tmp, `${JSON.stringify(observation, null, 2)}\n`, { flag: "wx", mode: 0o600 })
      await fs.rename(tmp, path)
      stream.stats.written++
    } catch (error) {
      await Promise.resolve(fs.rm(tmp, { force: true })).catch(() => {})
      throw error
    }
  }

  const run = async () => {
    while (stream.pending !== null) {
      const job = stream.pending
      stream.pending = null
      try {
        await write(job)
      } catch {
        stream.stats.writeErrors++
      }
    }
    stream.runner = null
  }

  /** Never throws. An older observation can never replace a newer accepted one. */
  stream.submit = (observation) => {
    const ms = Date.parse(observation.observed_at)
    if (!Number.isFinite(ms) || ms < stream.newestMs) {
      stream.stats.droppedOlder++
      return
    }
    if (stream.pending !== null) stream.stats.coalesced++
    stream.newestMs = ms
    stream.pending = { ms, observation }
    stream.runner ??= run()
  }

  stream.drain = async () => {
    while (stream.runner !== null) await stream.runner
  }

  return stream
}

export function createBridge({ config, fs = defaultFs, now = Date.now, registry = getRegistry() } = {}) {
  const cfg = config ?? resolveConfig()
  const seen = new WeakSet()
  const stats = { observed: 0, ignoredNoWindow: 0 }
  let disposed = false
  let stream = null

  if (cfg.outputFile !== null) {
    stream = registry.get(cfg.outputFile)
    if (!stream) {
      stream = createStream(cfg.outputFile, fs)
      registry.set(cfg.outputFile, stream)
    }
    stream.refs++
  }

  /** Passive hook body. Synchronous, returns nothing, never throws. */
  const handle = (event) => {
    try {
      if (disposed || stream === null || event === null || typeof event !== "object") return
      if (event.model?.providerID !== PROVIDER_ID) return
      if (seen.has(event)) return
      seen.add(event)

      const response = event.response
      if (response === null || typeof response !== "object") return
      const observation = buildBridgeObservation({
        url: event.request?.url,
        status: response.status,
        headers: response.headers,
        observedAt: now(),
      })
      if (observation === null) {
        stats.ignoredNoWindow++
        return
      }
      stats.observed++
      stream.submit(observation)
    } catch {
      // Observation must never disturb the model call.
    }
  }

  let released = false
  return {
    handle,
    config: cfg,
    stats: () => ({ ...stats, ...(stream?.stats ?? {}), pending: stream !== null && stream.pending !== null ? 1 : 0 }),
    drain: () => stream?.drain() ?? Promise.resolve(),
    /** Stops observing and waits for writes already accepted; never writes a "stale" marker. */
    dispose: async () => {
      disposed = true
      if (stream === null) return
      await stream.drain()
      if (!released) {
        released = true
        if (--stream.refs <= 0 && stream.runner === null && registry.get(stream.path) === stream) registry.delete(stream.path)
      }
    },
  }
}

/**
 * Plugin body, free of host imports so it can be tested with a fake context. Startup does no
 * I/O at all: it neither reads nor touches the output file.
 */
export async function setupBridge(ctx, deps = {}) {
  const bridge = createBridge({ config: resolveConfig(ctx?.options), ...deps })
  let registration = null
  if (bridge.config.outputFile !== null) {
    try {
      registration = await ctx.session.hook("http.response", bridge.handle, { providerID: PROVIDER_ID })
    } catch {
      // A failed registration only means no observations.
    }
  }

  return async () => {
    const disposing = bridge.dispose()
    await Promise.resolve(registration?.dispose?.()).catch(() => {})
    await disposing
  }
}
