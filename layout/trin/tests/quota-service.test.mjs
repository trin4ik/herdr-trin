import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const read = file => fs.readFileSync(new URL(`../mods/trin-quotas/${file}`, import.meta.url), "utf8");
const data = await import(`data:text/javascript,${encodeURIComponent(read("data.js"))}`);
const metrics = await import(`data:text/javascript,${encodeURIComponent(read("metrics.js"))}`);
const fixture = JSON.parse(fs.readFileSync(new URL("./fixtures/quota-v1.json", import.meta.url), "utf8"));
const packet = () => structuredClone(fixture);
const scope = { ...data };
vm.runInNewContext(read("monitor.js").replace(/^import .*;\n/gm, "").replaceAll("export ", "") + "\nthis.Monitor = QuotaMonitor;", scope);
const transport = {};
vm.runInNewContext(read("transport.js").replace(/^import .*;\n/gm, "").replaceAll("export ", "") + "\nthis.parse = parseResponse; this.url = quotaURL; this.request = requestQuotas;", transport);
function cx() {
  return { tasks: [], notifications: 0, spawn(fn) { this.tasks.push(fn(this)); }, notify() { this.notifications++; } };
}
async function refresh(monitor, context) { monitor.refresh(context); await Promise.all(context.tasks); }

test("live contract maps aliases, stable sidebar order, primary windows and scoped months", () => {
  const result = data.normalizeSnapshot(packet());
  assert.deepEqual(result.subscriptions.map(s => s.provider), ["openai", "anthropic", "ollama", "kimi"]);
  const [codex, claude, ollama, kimi] = result.subscriptions;
  assert.equal(metrics.primaryWindow(codex).label, "Неделя");
  assert.equal(metrics.primaryWindow(claude).label, "5 часов");
  assert.equal(metrics.primaryWindow(ollama).label, "Сессия");
  assert.equal(metrics.primaryWindow(kimi).usedPercent, 0.06);
  assert.equal(metrics.percent(metrics.usage(metrics.primaryWindow(kimi))), "0.06%");
  assert.deepEqual(kimi.windows.slice(1).map(w => w.label), ["Месяц · код", "Месяц · всего"]);
  assert.equal(codex.balances[0].value, 10);
});
test("fractional usage, known zero and unavailable usage are not confused", () => {
  const p = packet();
  p.subscriptions[0].quotas[0].used_percent = null;
  p.subscriptions[0].quotas[0].remaining_percent = 63;
  assert.equal(data.normalizeSnapshot(p).subscriptions[0].windows[0].usedPercent, 37);
  p.subscriptions[0].quotas[0].remaining_percent = null;
  assert.equal(data.normalizeSnapshot(p).subscriptions[0].windows[0].usedPercent, null);
  const ollama = data.normalizeSnapshot(p).subscriptions[2];
  assert.equal(ollama.windows[0].usedPercent, 0);
  assert.equal(metrics.remaining(ollama.windows[1]), 99.45);
});
test("optional account_email extension is provider-independent and never inferred from sources or IDs", () => {
  const p = packet();
  for (const s of p.subscriptions) { s.account_email = "  account@example.test  "; s.plan = "pro"; }
  for (const s of data.normalizeSnapshot(p).subscriptions) {
    assert.equal(s.accountEmail, "account@example.test"); assert.equal(s.plan, "pro");
  }
  for (const invalid of [undefined, null, {}, 42, "", "  ", "account\n@example.test", "account\u0000@example.test"]) {
    for (const s of p.subscriptions) { s.account_email = invalid; s.source = "do-not-infer@example.test"; }
    for (const s of data.normalizeSnapshot(p).subscriptions) assert.equal(s.accountEmail, null);
  }
  p.subscriptions[0].account_email = "a".repeat(1000);
  assert.equal(data.normalizeSnapshot(p).subscriptions.find(s => s.id === p.subscriptions[0].id).accountEmail.length, 300);
});
test("schema/version, generated time and duplicate identities are validated", () => {
  assert.throws(() => data.normalizeSnapshot({ ...packet(), schema_version: 2 }));
  assert.throws(() => data.normalizeSnapshot({ ...packet(), generated_at: "bad" }));
  const p = packet(); p.subscriptions.push(p.subscriptions[0]);
  assert.throws(() => data.normalizeSnapshot(p));
  const q = packet(); q.subscriptions[0].quotas.push(q.subscriptions[0].quotas[0]);
  assert.throws(() => data.normalizeSnapshot(q));
});
test("HTTP errors and malformed bodies never become successful empty snapshots", () => {
  assert.equal(transport.parse({ code: 0, stdout: '{"ok":true}\n200' }).ok, true);
  for (const response of [{ code: 28, stdout: "\n000" }, { code: 0, stdout: "private error\n503" },
    { code: 0, stdout: "not JSON\n200" }, { code: 0, stdout: "{}\n302" }]) {
    assert.throws(() => transport.parse(response));
  }
});
test("public transport is local by default and rejects plaintext remote/secret URLs", async () => {
  assert.equal(transport.url(), "http://127.0.0.1:8098/v1/quotas");
  assert.equal(transport.url("https://quota.example.com/v1/quotas"), "https://quota.example.com/v1/quotas");
  for (const url of ["http://example.com/v1/quotas", "https://user:pass@example.com/v1/quotas",
    "https://example.com/v1/quotas?token=secret", "file:///private", "https://example.com/v1/quotas#private"]) {
    assert.throws(() => transport.url(url));
  }
  let args;
  transport.exec = async (cmd, argv) => { assert.equal(cmd, "curl"); args = argv; return { code: 0, stdout: "{}\n200" }; };
  await transport.request();
  assert.equal(args[0], "--disable");
  assert.equal(args[args.indexOf("--proto") + 1], "=http");
  assert.ok(!args.includes("--location"));
  await transport.request("https://quota.example.com/v1/quotas");
  assert.equal(args[args.indexOf("--proto") + 1], "=https");
});
test("one in-flight request and closed views cannot publish or reconnect", async () => {
  let release, calls = 0;
  const m = new scope.Monitor(() => { calls++; return new Promise(resolve => { release = resolve; }); });
  const c = cx(); m.refresh(c); m.refresh(c);
  assert.equal(calls, 1);
  m.close(); release(packet()); await Promise.all(c.tasks);
  assert.equal(m.subscriptions.length, 0);
  assert.equal(c.notifications, 0);
  m.refresh(c); assert.equal(calls, 1);
});
test("transport failure keeps the last snapshot and recovers on the next poll", async () => {
  let fail = false;
  const m = new scope.Monitor(async () => { if (fail) throw new Error("HTTP 503"); return packet(); });
  const c = cx(); await refresh(m, c);
  const previous = m.subscriptions;
  fail = true; await refresh(m, c);
  assert.equal(m.subscriptions, previous);
  assert.equal(m.error.message, "HTTP 503");
  assert.ok(m.fetchedAt);
  fail = false; await refresh(m, c);
  assert.equal(m.error, null);
});
test("bad or older snapshots cannot overwrite good data", async () => {
  let response = packet();
  const m = new scope.Monitor(async () => response), c = cx();
  await refresh(m, c); const previous = m.subscriptions;
  response = { ...packet(), schema_version: 99 }; await refresh(m, c);
  assert.equal(m.subscriptions, previous);
  response = { ...packet(), generated_at: "2026-11-03T10:00:00Z" }; await refresh(m, c);
  assert.equal(m.subscriptions, previous);
  assert.ok(m.error);
});
test("provider failure preserves last-good quotas; successful removal stays removed", async () => {
  let response = packet();
  const m = new scope.Monitor(async () => response), c = cx(); await refresh(m, c);
  response = packet(); Object.assign(response.subscriptions[2], { status: "error", error: { http_status: 429, message: "Rate limited" }, quotas: [], last_success_at: null });
  await refresh(m, c);
  const kimi = m.subscriptions.find(s => s.provider === "kimi");
  assert.equal(kimi.windows.length, 3);
  assert.equal(kimi.windows[0].usedPercent, 0.06);
  assert.equal(kimi.error.status, 429);
  assert.equal(kimi.observedAt, "2026-11-04T09:58:00Z");
  response = { ...packet(), subscriptions: [] }; await refresh(m, c);
  assert.equal(m.subscriptions.length, 0);
  assert.equal(m.error, null);
});
test("freshness expires locally and transport errors stay separate from provider timestamps", () => {
  const s = data.normalizeSnapshot(packet()).subscriptions[0];
  assert.equal(metrics.hasWarning(s, "2026-11-04T10:10:00Z"), false);
  assert.equal(metrics.hasWarning(s, "2026-11-04T10:30:00Z"), true);
  const tip = metrics.quotaTooltip(s, "2026-11-04T10:10:00Z", { fetchedAt: "2026-11-04T10:00:00Z",
    error: { message: "HTTP 503", at: "2026-11-04T10:01:00Z" } });
  assert.ok(tip.includes("12:58 МСК"));
  assert.ok(tip.includes("Связь с сервисом: HTTP 503"));
  assert.ok(tip.includes("Неудачная попытка Herdr: 04.11.2026, 13:01 МСК"));
  assert.equal(tip.includes("ДЕМО"), false);
});

test("a provider without quota history can report null windows without hiding healthy peers", () => {
  const p = packet();
  Object.assign(p.subscriptions[2], { status: "error", quotas: null, last_success_at: null,
    error: { code: "auth_expired", message: "Авторизация истекла" } });
  const result = data.normalizeSnapshot(p), kimi = result.subscriptions[3];
  assert.equal(result.subscriptions[0].windows[0].usedPercent, 37);
  assert.equal(kimi.windows.length, 0);
  assert.equal(metrics.usage(metrics.primaryWindow(kimi)), null);
  assert.ok(metrics.quotaTooltip(kimi, fixture.generated_at).includes("Авторизация истекла"));
});

test("actual view lifecycle requests immediately, registers a minute timer and closes its monitor", async () => {
  let requests = 0, timer;
  const context = { View: class {}, QuotaMonitor: scope.Monitor, REFRESH_MS: 60000,
    shared: () => null,
    nextCommand: () => new Promise(() => {}),
    requestQuotas: async () => { requests++; return packet(); } };
  vm.runInNewContext(read("main.js").replace(/^import .*;\n/gm, "").replace("export default class", "class") + "\nthis.Panel = Quotas;", context);
  const panel = new context.Panel(), c = cx();
  c.timer = { every(ms, callback) { assert.equal(ms, 60000); timer = callback; } };
  c.timer.after = (_ms, callback) => callback(c);
  panel.init({}, c); await c.tasks[0];
  assert.equal(requests, 1);
  assert.equal(panel.monitor.subscriptions.length, 4);
  panel.setOpen(panel.monitor.subscriptions[0].id, true, c);
  assert.equal(requests, 1);
  timer(c); await c.tasks.at(-1);
  assert.equal(requests, 2);
  panel.close(); timer(c);
  assert.equal(requests, 2);
});

test("private endpoint changes replace the cache and reject late replies from the previous service", async () => {
  let command, endpoint = null, release, requested;
  const pending = [];
  const context = { View: class {}, QuotaMonitor: scope.Monitor, REFRESH_MS: 60000,
    shared: () => endpoint, setShared: (_key, value) => { endpoint = value; }, quotaURL: transport.url,
    nextCommand: () => new Promise(resolve => { command = resolve; }),
    requestQuotas: value => { requested = value; return new Promise(resolve => { release = resolve; pending.push(resolve); }); } };
  vm.runInNewContext(read("main.js").replace(/^import .*;\n/gm, "").replace("export default class", "class") + "\nthis.Panel = Quotas;", context);
  const panel = new context.Panel(), c = cx();
  c.timer = { every() {} };
  panel.init({}, c);
  const old = panel.monitor;
  command("endpoint--" + encodeURIComponent("https://quota.example.com/v1/quotas"));
  await new Promise(setImmediate);
  assert.equal(endpoint, "https://quota.example.com/v1/quotas");
  assert.equal(requested, endpoint);
  assert.notEqual(panel.monitor, old);
  pending[0](packet()); await new Promise(setImmediate);
  assert.equal(old.subscriptions.length, 0);
  release(packet()); await new Promise(setImmediate);
  command("endpoint--" + encodeURIComponent("http://untrusted.example/v1/quotas")); await new Promise(setImmediate);
  assert.equal(endpoint, "https://quota.example.com/v1/quotas");
  command("reset-endpoint"); await new Promise(setImmediate);
  assert.equal(endpoint, null);
  panel.close();
});

test("normalizer preserves a reported start time and leaves absent start unknown", () => {
  const p = packet();
  p.subscriptions[0].quotas[0].window.started_at = "2026-11-03T10:00:00Z";
  const snapshot = data.normalizeSnapshot(p);
  assert.equal(snapshot.subscriptions[0].windows[0].startedAt, "2026-11-03T10:00:00Z");
  assert.equal(snapshot.subscriptions[1].windows[0].startedAt, null);
});
