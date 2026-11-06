import { agents, terminals, panes, spaces, exec, readTextFile, appHome } from "herdr";

const cleanTitle = title => (title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();

export class ActivityMonitor {
  constructor() {
    this.results = {};
    this.good = {};
    this.zeroes = {};
    this.busy = new Set();
    this.requests = new Map();
    this.inFlight = new Map();
    this.targetKeys = new Map();
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
    const groups = new Map();
    const list = agents().filter(a => a.app === "opencode" || a.program?.key === "opencode");
    this.prune(new Set(list.map(a => a.address)));
    for (const a of list) {
      const machine = sp.get(a.space)?.machine ?? null;
      const records = ts.get(a.address)?.records ?? {};
      const previous = this.good[a.address];
      const hints = Object.entries(records).filter(([id, r]) => id.startsWith("ses") && r.title === cleanTitle(a.title)).map(([id]) => id);
      if (previous?.title === a.title && previous.sessionID) hints.push(previous.sessionID);
      const target = {
        address: a.address,
        title: a.title,
        cwd: ps.get(a.pane)?.cwd,
        hints: [...new Set(hints)],
        durationState: previous?.title === a.title ? previous.meta?.durationState : undefined,
        metricsCache: previous?.title === a.title ? previous.meta?.metricsCache : undefined,
      };
      if (!groups.has(machine)) groups.set(machine, []);
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
    }
    for (const [machine, targets] of groups) {
      const signature = JSON.stringify(targets.map(({ address, title, cwd }) => [address, title, cwd]));
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
      this.startedAt.set(machine, Date.now());
      this.requests.set(machine, request);
      this.busy.add(machine);
      this.inFlight.set(machine, (this.inFlight.get(machine) ?? 0) + 1);
      cx.spawn(async cx => {
        const next = {};
        let connected = false;
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
          const result = await exec("python3", ["-c", code, ...args], { on: machine });
          if (result.code !== 0) throw new Error("Task reader failed");
          const payload = JSON.parse(result.stdout);
          const response = payload.snapshot;
          connected = payload.connected === true;
          if (!this.closed && this.requests.get(machine) === request) {
            this.bridgeKeys.set(machine, payload.key);
            if (connected && payload.state) this.bridgeStates.set(machine, payload.state);
            else this.bridgeStates.delete(machine);
          }
          for (const info of Object.values(response)) if (info.meta?.durationRunning) info.meta.durationAt = Date.now();
          for (const target of targets) next[target.address] = response[target.address]
            ?? { title: target.title, error: "Task reader returned no session data" };
        } catch {
          if (!this.closed && this.requests.get(machine) === request) this.bridgeStates.delete(machine);
          for (const target of targets) next[target.address] = { title: target.title, error: "OpenCode task API is unavailable" };
        } finally {
          this.inFlight.set(machine, this.inFlight.get(machine) - 1);
          if (this.requests.get(machine) === request) this.busy.delete(machine);
        }
        if (this.closed) return;
        if (this.requests.get(machine) !== request) {
          this.refresh(cx);
          return;
        }
        if (this.accept(next)) cx.notify();
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
    // In-flight bounded exec/subscriptions expire themselves within 27s;
    // closed prevents publication or reconnect, including after hot reload.
  }

  stale(info, good) {
    return Boolean(info.error) || Date.now() - good.checkedAt > (good.eventBridge ? 45000 : 20000);
  }

  get(agent) {
    const info = this.results[agent.address];
    if (!info || info.title !== agent.title) return null;
    const good = this.good[agent.address];
    if (!good || good.title !== agent.title) return null;
    if (!(good.subagents > 0 || good.shells > 0)) return null;
    const stale = this.stale(info, good);
    return { ...good, stale };
  }

  details(agent) {
    const info = this.results[agent.address];
    const good = this.good[agent.address];
    if (!info || info.title !== agent.title || good?.title !== agent.title || !good.meta) return null;
    return { ...good.meta, sessionID: good.sessionID, stale: this.stale(info, good) };
  }

  notice(agent) {
    const info = this.results[agent.address];
    if (!info || info.title !== agent.title) return "";
    const good = this.good[agent.address];
    if (info.error || (good && this.stale(info, good))) return this.get(agent)
      ? "Last known task counts; refresh is paused or unavailable"
      : "Task counts unavailable (not zero)";
    return good?.pendingZero?.length ? "Confirming task completion on the next refresh" : "";
  }

  accept(next) {
    Object.assign(this.results, next);
    for (const [address, info] of Object.entries(next)) {
      const old = this.good[address];
      const same = old?.title === info.title && (!info.sessionID || !old.sessionID || old.sessionID === info.sessionID);
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
    for (const address of Object.keys(this.results)) if (!addresses.has(address)) {
      delete this.results[address];
      delete this.good[address];
      delete this.zeroes[address];
    }
  }
}
