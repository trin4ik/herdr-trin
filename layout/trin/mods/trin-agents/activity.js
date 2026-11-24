import { agents, terminals, panes, spaces, machines, exec, readTextFile, appHome } from "herdr";

export const sessionAddress = (machine, sessionID) => `opencode-session:${encodeURIComponent(machine ?? "")}/${sessionID}`;

export const isOpenCode = (agent, terminal) => agent.app === "opencode" || agent.program?.key === "opencode"
  || (terminal?.report ?? terminal?.status)?.app === "opencode";
const validID = id => typeof id === "string" && /^ses[A-Za-z0-9_-]+$/.test(id);
const captionOf = title => (title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
const sameCaption = (caption, title) => Boolean(caption && title && (caption === title ||
  (caption.endsWith("…") && caption.length >= 17 && title.startsWith(caption.slice(0, -1).trimEnd()))));

function launchedSessionID(terminal) {
  const argv = terminal?.foreground?.argv;
  if (!Array.isArray(argv) || argv[0]?.split(/[\\/]/).at(-1) !== "opencode"
    || argv.some(arg => arg === "--standalone" || arg === "--server" || arg.startsWith("--server="))) return null;
  const ids = argv.flatMap((arg, i) => arg === "-s" || arg === "--session" ? [argv[i + 1]]
    : arg.startsWith("--session=") ? [arg.slice(10)] : []).filter(validID);
  return ids.length === 1 ? ids[0] : null;
}

export function terminalSessionID(terminal) {
  const status = terminal?.report ?? terminal?.status;
  if (status?.app && status.app !== "opencode") return null;
  const entries = Object.entries(terminal?.records ?? {}).filter(([id, report]) =>
    validID(id) && report?.app === "opencode");
  // Herdr can retain multiple reports for a terminal. Only trust the record
  // selected by its aggregate status. Even a single record can be from an
  // earlier session while a newly active session has no keyed report yet.
  if (status?.app !== "opencode" || (!status.state && !status.title)) return entries.length === 1 ? entries[0][0] : null;
  const chosen = entries.filter(([, report]) => report.state === status.state && report.title === status.title);
  return chosen.length === 1 ? chosen[0][0] : null;
}

export class ActivityMonitor {
  constructor(titles = {}) {
    this.results = {};
    this.good = {};
    this.zeroes = {};
    this.busy = new Set();
    this.requests = new Map();
    this.inFlight = new Map();
    this.targetKeys = new Map();
    this.desiredKeys = new Map();
    this.background = new Map();
    this.bindings = new Map(Object.entries(titles).filter(([, item]) =>
      validID(item?.sessionID) && Number.isFinite(item.pid) && Number.isFinite(item.created))
      .map(([address, item]) => [address, { id: item.sessionID, pid: item.pid,
        created: item.created, caption: captionOf(item.caption), title: item.title }]));
    this.signature = "";
    this.bridgeKeys = new Map();
    this.bridgeStates = new Map();
    this.reconnectTimers = new Map();
    this.startedAt = new Map();
    this.retryAt = new Map();
    this.failures = new Map();
    this.closed = false;
  }

  refresh(cx) {
    if (this.closed) return;
    const sp = new Map(spaces().map(s => [s.id, s]));
    const ps = new Map(panes().map(p => [p.pane, p]));
    const ts = new Map(terminals().map(t => [t.address, t]));
    // Discovery must work even on a ready machine with no Herdr terminals.
    const groups = new Map(machines().filter(m => m.layout === "ready").map(m => [m.name ?? null, []]));
    const list = agents().filter(a => !ts.get(a.address)?.parent && isOpenCode(a, ts.get(a.address)));
    this.prune(new Set(list.map(a => a.address)));
    for (const a of list) {
      const machine = sp.get(a.space)?.machine ?? null;
      if (!groups.has(machine)) continue;
      const sessionID = this.boundSessionID(a, ts.get(a.address), true);
      const previous = this.good[a.address];
      const reusable = sessionID ? previous?.sessionID === sessionID : previous?.title === a.title;
      const target = {
        address: a.address,
        title: a.title,
        cwd: ps.get(a.pane)?.cwd,
        sessionID,
        durationState: reusable ? previous?.meta?.durationState : undefined,
        metricsCache: reusable ? previous?.meta?.metricsCache : undefined,
      };
      groups.get(machine).push(target);
    }
    // A changed target set must not wait for the old 18s SSE lease. Retire its
    // response and start the new set immediately. At most two bounded calls
    // per machine (current + retiring); further changes coalesce until one ends.
    for (const machine of this.requests.keys()) if (!groups.has(machine)) {
      this.requests.delete(machine);
      this.busy.delete(machine);
      this.targetKeys.delete(machine);
      this.bridgeKeys.delete(machine);
      this.bridgeStates.delete(machine);
      this.reconnectTimers.get(machine)?.cancel();
      this.reconnectTimers.delete(machine);
    }
    this.desiredKeys = new Map([...groups].map(([machine, targets]) => [machine,
      JSON.stringify(targets.map(({ address, title, cwd, sessionID }) => [address, title, cwd, sessionID]))]));
    for (const machine of this.background.keys()) if (!groups.has(machine)) {
      this.background.delete(machine);
      cx.notify();
    }
    for (const [machine, targets] of groups) {
      const signature = this.desiredKeys.get(machine);
      const changed = signature !== this.targetKeys.get(machine);
      if (this.busy.has(machine) && this.requests.get(machine)?.signature === signature) continue;
      if ((this.inFlight.get(machine) ?? 0) >= 2) continue;
      if (!changed && Date.now() < (this.retryAt.get(machine) ?? 0)) continue;
      const wait = 1000 - (Date.now() - (this.startedAt.get(machine) ?? -Infinity));
      if (wait > 0) {
        if (!this.reconnectTimers.has(machine)) {
          this.reconnectTimers.set(machine, cx.timer.after(wait, cx => {
            this.reconnectTimers.delete(machine);
            this.refresh(cx);
          }));
        }
        continue;
      }
      if (changed) {
        this.targetKeys.set(machine, signature);
        this.bridgeKeys.delete(machine);
        this.retryAt.delete(machine);
        this.failures.delete(machine);
      }
      const request = { signature };
      const current = () => !this.closed && this.requests.get(machine) === request
        && this.desiredKeys.get(machine) === signature;
      this.startedAt.set(machine, Date.now());
      this.requests.set(machine, request);
      this.busy.add(machine);
      this.inFlight.set(machine, (this.inFlight.get(machine) ?? 0) + 1);
      cx.spawn(async cx => {
        const next = {};
        let connected = false;
        let background = [];
        try {
          this.scriptPromise ??= readTextFile(`${appHome()}/mods/trin-agents/activity.py`).catch(() => null);
          const script = await this.scriptPromise;
          if (!script) {
            this.scriptPromise = null;
            throw new Error("Missing task reader");
          }
          this.bridgePromise ??= readTextFile(`${appHome()}/mods/trin-agents/bridge.py`).catch(() => null);
          const bridge = await this.bridgePromise;
          if (!bridge) {
            this.bridgePromise = null;
            throw new Error("Missing event bridge");
          }
          // Inject the reader as a module: no remote install or credential
          // copying. The bridge itself is a separate, testable source file.
          const code = `import types,sys\nm=types.ModuleType("trin_activity")\nsys.modules[m.__name__]=m\nexec(${JSON.stringify(script)},m.__dict__)\n` + bridge;
          const args = [JSON.stringify(targets), JSON.stringify(this.bridgeKeys.get(machine) ?? null),
            JSON.stringify(this.bridgeStates.get(machine) ?? {})];
          const result = await exec("python3", ["-c", code, ...args], { on: machine, timeout: 27000 });
          if (result.code !== 0) throw new Error("Task reader failed");
          const payload = JSON.parse(result.stdout);
          const response = payload.snapshot;
          connected = payload.connected === true;
          background = payload.background ?? [];
          if (current()) {
            this.bridgeKeys.set(machine, payload.key);
            if (connected && payload.state) this.bridgeStates.set(machine, payload.state);
            else this.bridgeStates.delete(machine);
          }
          for (const info of Object.values(response)) if (info.meta?.durationRunning) info.meta.durationAt = Date.now();
          for (const target of targets) next[target.address] = {
            sessionID: target.sessionID,
            ...(response[target.address] ?? { title: target.title, error: "Task reader returned no session data" }),
          };
        } catch {
          if (current()) this.bridgeStates.delete(machine);
          for (const target of targets) next[target.address] = { title: target.title, sessionID: target.sessionID,
            error: "OpenCode task API is unavailable" };
        } finally {
          this.inFlight.set(machine, this.inFlight.get(machine) - 1);
          if (this.requests.get(machine) === request) this.busy.delete(machine);
        }
        if (this.closed) return;
        if (!current()) {
          this.refresh(cx);
          return;
        }
        const changedBackground = JSON.stringify(this.background.get(machine) ?? []) !== JSON.stringify(background);
        if (background.length) this.background.set(machine, background);
        else this.background.delete(machine);
        if (this.accept(next) || changedBackground) cx.notify();
        if (connected) {
          this.failures.delete(machine);
          this.retryAt.delete(machine);
          // A second independent snapshot confirms zero immediately, rather
          // than leaving a completed task badge until lease renewal.
          if (targets.some(t => this.good[t.address]?.pendingZero?.length)) this.bridgeKeys.delete(machine);
          this.refresh(cx);
        } else {
          const failures = (this.failures.get(machine) ?? 0) + 1;
          this.failures.set(machine, failures);
          this.retryAt.set(machine, Date.now() + Math.min(30000, 2000 * 2 ** Math.min(failures, 4)));
        }
      });
    }
  }

  close() {
    this.closed = true;
    for (const timer of this.reconnectTimers.values()) timer.cancel();
    this.reconnectTimers.clear();
    this.bridgeStates.clear();
    this.background.clear();
    // In-flight bounded exec/subscriptions expire themselves within 27s;
    // closed prevents publication or reconnect, including after hot reload.
  }

  stale(info, good) {
    return Boolean(info.error) || Date.now() - good.checkedAt > (good.eventBridge ? 45000 : 20000);
  }

  boundSessionID(agent, terminal, forLookup = false) {
    const address = agent.address;
    if (!terminal || !isOpenCode(agent, terminal)) return null;
    const pid = terminal.foreground?.pid ?? terminal.pid ?? null;
    const created = terminal.created_ms ?? null;
    const caption = captionOf(agent.title);
    let old = this.bindings.get(address);
    if (old && (old.pid !== pid || old.created !== created)) {
      this.bindings.delete(address);
      old = null;
    }
    const observed = terminalSessionID(terminal);
    const launched = !old && launchedSessionID(terminal);
    const id = observed || launched;
    if (id) {
      this.bindings.set(address, { id, pid, created, caption, title: old?.id === id ? old.title : undefined });
      return id;
    }
    // An unkeyed working report may be the first turn of a different session.
    // Keep the last proven binding only while idle, never to merge that turn.
    if (!old || agent.status === "working" || agent.status === "blocked") return null;
    const current = this.session(agent, old.id)?.sessionTitle;
    if (caption === old.caption || sameCaption(caption, current)) {
      old.caption = caption;
      return old.id;
    }
    // Allow the server rename to catch up without ever painting a stale
    // title over a terminal that may have switched to another idle session.
    return forLookup ? old.id : null;
  }

  session(agent, sessionID) {
    const info = this.results[agent.address];
    const known = info?.sessionID && (Object.hasOwn(info, "sessionTitle") || !info.error) ? info : this.good[agent.address];
    if (!sessionID) return null;
    if (known?.sessionID === sessionID) return known;
    const binding = this.bindings.get(agent.address);
    return binding?.id === sessionID && binding.title ? { sessionID, sessionTitle: binding.title } : null;
  }

  backgroundRows() {
    return [...this.background].flatMap(([machine, sessions]) => sessions.map(s => ({
      ...s, machine, address: sessionAddress(machine, s.sessionID),
      program: { name: "OpenCode" }, status: s.status ?? "working", background: true,
    })));
  }

  get(agent, sessionID) {
    const info = this.results[agent.address];
    if (!info || (sessionID ? info.sessionID !== sessionID : info.title !== agent.title)) return null;
    const good = this.good[agent.address];
    if (!good || (sessionID ? good.sessionID !== sessionID : good.title !== agent.title)) return null;
    if (!(good.subagents > 0 || good.shells > 0)) return null;
    const stale = this.stale(info, good);
    return { ...good, stale };
  }

  details(agent, sessionID) {
    const info = this.results[agent.address];
    const good = this.good[agent.address];
    if (!info || !good?.meta || (sessionID ? info.sessionID !== sessionID || good.sessionID !== sessionID
      : info.title !== agent.title || good.title !== agent.title)) return null;
    return { ...good.meta, sessionID: good.sessionID, stale: this.stale(info, good) };
  }

  notice(agent, sessionID) {
    const info = this.results[agent.address];
    if (!info || (sessionID ? info.sessionID !== sessionID : info.title !== agent.title)) return "";
    const good = this.good[agent.address];
    if (info.error || (good && this.stale(info, good))) return this.get(agent, sessionID)
      ? "Last known task counts; refresh is paused or unavailable"
      : "Task counts unavailable (not zero)";
    return good?.pendingZero?.length ? "Confirming task completion on the next refresh" : "";
  }

  accept(next) {
    Object.assign(this.results, next);
    for (const [address, info] of Object.entries(next)) {
      const old = this.good[address];
      const same = old && (old.sessionID && info.sessionID
        ? old.sessionID === info.sessionID : old.title === info.title);
      if (!same) {
        delete this.good[address];
        delete this.zeroes[address];
      }
      if (info.error) {
        delete this.zeroes[address];
        continue;
      }
      const good = { ...info, checkedAt: Date.now(), pendingZero: [] };
      if (same && info.meta?.durationError && old.meta) {
        good.meta = { ...info.meta, durationMs: old.meta.durationMs,
          durationReady: old.meta.durationReady, durationAt: old.meta.durationAt,
          durationRunning: false, durationStale: true };
      }
      const zeroes = this.zeroes[address] ??= {};
      for (const kind of ["subagents", "shells"]) {
        if (same && old[kind] > 0 && info[kind] === 0) {
          zeroes[kind] = (zeroes[kind] ?? 0) + 1;
          // One transient empty snapshot is not proof the tasks ended. Confirm
          // zero twice; new positive counts still appear immediately.
          if (zeroes[kind] < 2) {
            good[kind] = old[kind];
            good.pendingZero.push(kind);
            if (kind === "subagents") {
              good.subagentsDirect = old.subagentsDirect;
              good.subagentsNested = old.subagentsNested;
              good.subagentDetails = old.subagentDetails;
            }
            if (kind === "shells") {
              good.shellsOwn = old.shellsOwn;
              good.shellsInherited = old.shellsInherited;
              good.shellDetails = old.shellDetails;
            }
          }
        } else zeroes[kind] = 0;
      }
      this.good[address] = good;
    }
    // Ignore check timestamps when deciding whether the native UI must redraw.
    const signature = JSON.stringify(Object.entries(this.results).map(([address, info]) => {
      const { checkedAt, ...visible } = this.good[address] ?? {};
      return [address, info, visible];
    }));
    const changed = signature !== this.signature;
    this.signature = signature;
    return changed;
  }

  prune(addresses) {
    for (const address of this.bindings.keys()) if (!addresses.has(address)) this.bindings.delete(address);
    for (const address of Object.keys(this.results)) if (!addresses.has(address)) {
      delete this.results[address];
      delete this.good[address];
      delete this.zeroes[address];
    }
  }
}
