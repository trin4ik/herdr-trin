import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("../mods/trin-projects/project-icons.js", import.meta.url), "utf8")
  .replace(/^import .*;\n/gm, "").replace(/^export /gm, "");
const local = { folder: "/repo" }, remote = { folder: "/repo", machine: "server" };
const machines = (...rows) => rows.map(name => ({ name, layout: "ready" }));
const packet = (...result) => ({ code: 0, stdout: JSON.stringify(result) });
const icon = Buffer.from("known test data").toString("base64");

function fixture(options = {}) {
  const calls = [], jobs = [], state = options.state ?? {}, files = new Map(options.files), events = [];
  let timer;
  const context = { Buffer, createHash, randomBytes, appHome: () => "/app", readTextFile: async () => "reader",
    shared: k => state[k], setShared: (k, v) => { state[k] = v; },
    readFile: async path => { if (!files.has(path)) throw Error("missing"); return files.get(path); },
    mkdir: async () => {},
    writeFile: async (path, bytes) => {
      if (options.fail) { options.fail = false; files.set(path, bytes.subarray(0, 2)); throw Error("partial"); }
      files.set(path, bytes);
    },
    unlink: async path => files.delete(path),
    watchFolder: path => {
      let finish;
      const promise = new Promise(resolve => { finish = resolve; });
      promise.cancel = () => finish([]);
      events.push({ path, finish });
      return promise;
    },
    exec: (cmd, args, opts) => {
      if (cmd === "/bin/mv") {
        files.set(args[2], files.get(args[1]));
        files.delete(args[1]);
        return Promise.resolve({ code: 0 });
      }
      return new Promise(resolve => calls.push({ args, opts, resolve }));
    },
  };
  vm.runInNewContext(source + "\nthis.Registry = ProjectIcons; this.iconFor = projectIcon;", context);
  const cx = { timer: { every(_ms, fn) { timer = fn; return { cancel() {} }; } },
    spawn: fn => { jobs.push(fn(cx)); }, notify() {} };
  return { registry: new context.Registry(), iconFor: context.iconFor, cx, calls, jobs, events, files,
    tick: () => timer(cx), yield: () => new Promise(resolve => setImmediate(resolve)) };
}

test("reads the owning machine, keeps last good when unavailable and removes confirmed missing icons", async () => {
  const f = fixture();
  f.registry.sync([local, remote], machines(null, "server"), f.cx);
  await f.yield();
  assert.deepEqual(f.calls.map(c => [c.opts.on, JSON.parse(c.args[3])]), [[null, ["/repo"]], ["server", ["/repo"]]]);
  assert.equal(f.calls[1].opts.timeout, 5000);
  f.calls[0].resolve(packet(""));
  f.calls[1].resolve(packet(icon));
  await Promise.all(f.jobs);
  const saved = f.iconFor(remote);
  assert.match(saved, /^data\/trin-project-icons\/[0-9a-f]{64}\.png$/);
  assert.equal(f.iconFor(local), null);
  f.registry.sync([local, remote], machines(null), f.cx);
  assert.equal(f.iconFor(remote), saved);
  f.registry.sync([local, remote], machines(null, "server"), f.cx);
  await f.yield();
  f.calls[2].resolve(packet(null));
  await f.jobs[2];
  assert.equal(f.iconFor(remote), saved);
  f.tick();
  await f.yield();
  f.calls[3].resolve(packet(""));
  f.calls[4].resolve(packet(""));
  await Promise.all(f.jobs);
  assert.equal(f.iconFor(remote), null);
  f.registry.close();
});

test("obsolete replies do not publish after reconnect or project removal", async () => {
  const f = fixture();
  f.registry.sync([remote], machines("server"), f.cx);
  await f.yield();
  f.registry.sync([remote], machines(), f.cx);
  f.registry.sync([remote], machines("server"), f.cx);
  f.calls[0].resolve(packet(icon));
  await f.jobs[0];
  assert.equal(f.iconFor(remote), null);
  await f.yield();
  f.registry.sync([], machines("server"), f.cx);
  f.calls[1].resolve(packet(icon));
  await f.jobs[1];
  assert.equal(f.iconFor(remote), null);
  f.registry.close();
});

test("local icons watch the exact file and clean up on close", async () => {
  const f = fixture();
  f.registry.sync([local], machines(null), f.cx);
  await f.yield();
  f.calls[0].resolve(packet(icon));
  await f.jobs[0];
  f.registry.sync([local], machines(null), f.cx);
  assert.equal(f.events[0].path, "/repo/.herdr");
  await f.yield();
  f.calls[1].resolve(packet(icon));
  await f.yield();
  f.events[0].finish(["/repo/.herdr/other"]);
  await f.yield();
  assert.equal(f.calls.length, 2);
  f.events[1].finish(["/repo/.herdr/icon.png"]);
  await f.yield();
  assert.equal(f.calls.length, 3);
  f.calls[2].resolve(packet(""));
  f.registry.close();
  await Promise.all(f.jobs);
});

test("the first read after subscribing catches removal in the startup gap", async () => {
  const f = fixture();
  f.registry.sync([local], machines(null), f.cx);
  await f.yield();
  f.calls[0].resolve(packet(icon));
  await f.jobs[0];
  f.registry.sync([local], machines(null), f.cx);
  await f.yield();
  f.calls[1].resolve(packet(""));
  await f.yield();
  assert.equal(f.iconFor(local), null);
  f.registry.close();
  await Promise.all(f.jobs);
});

test("a watched project retries a transient read failure without another file event", async () => {
  const f = fixture();
  f.registry.sync([local], machines(null), f.cx);
  await f.yield();
  f.calls[0].resolve(packet(icon));
  await f.jobs[0];
  f.registry.sync([local], machines(null), f.cx);
  await f.yield();
  f.calls[1].resolve(packet(null));
  await f.yield();
  assert.ok(f.iconFor(local));
  f.tick();
  await f.yield();
  f.calls[2].resolve(packet(""));
  await f.yield();
  assert.equal(f.iconFor(local), null);
  f.registry.close();
  await Promise.all(f.jobs);
});

test("a partial cache write is never published and can be retried", async () => {
  const f = fixture({ fail: true });
  f.registry.sync([remote], machines("server"), f.cx);
  await f.yield();
  f.calls[0].resolve(packet(icon));
  await f.jobs[0];
  assert.equal(f.iconFor(remote), null);
  assert.equal(f.files.size, 0);
  f.tick();
  await f.yield();
  f.calls[1].resolve(packet(icon));
  await f.jobs[1];
  assert.equal(f.files.get(`/app/${f.iconFor(remote)}`).toString("base64"), icon);
  f.registry.close();
});

test("offline startup checks local cached bytes before publishing a path", async () => {
  const raw = Buffer.from(icon, "base64"), key = JSON.stringify(["server", "/repo"]);
  const saved = `data/trin-project-icons/${createHash("sha256").update(raw).digest("hex")}.png`;
  for (const bytes of [undefined, raw.subarray(0, 3), raw]) {
    const f = fixture({ state: { "trin.project-icons.v1": { [key]: saved }, "trin.project-icons.ready": { [key]: saved } },
      files: bytes ? [[`/app/${saved}`, bytes]] : [] });
    f.registry.sync([remote], machines(), f.cx);
    assert.equal(f.iconFor(remote), null, "do not paint the saved path until it has been checked");
    await Promise.all(f.jobs);
    assert.equal(f.iconFor(remote), bytes === raw ? saved : null);
    assert.equal(f.calls.length, 0);
    f.registry.close();
  }
});

test("deleting a previously verified local copy falls back even while the machine stays offline", async () => {
  const raw = Buffer.from(icon, "base64"), key = JSON.stringify(["server", "/repo"]);
  const path = `data/trin-project-icons/${createHash("sha256").update(raw).digest("hex")}.png`;
  const f = fixture({ state: { "trin.project-icons.v1": { [key]: path } }, files: [[`/app/${path}`, raw]] });
  f.registry.sync([remote], machines(), f.cx);
  await Promise.all(f.jobs);
  assert.equal(f.iconFor(remote), path);
  f.files.delete(`/app/${path}`);
  f.tick();
  await Promise.all(f.jobs);
  assert.equal(f.iconFor(remote), null);
  assert.equal(f.calls.length, 0);
  f.registry.close();
});
