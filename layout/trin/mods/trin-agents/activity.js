import { agents, terminals, panes, spaces, machines, exec, readTextFile, appHome } from "herdr";
import { attachedAgents, isOpenCode, serviceProbe, sessionAddress, terminalLaunch, terminalReportAdvanced, terminalReportVersion, terminalSessionID, validID } from "./identity.js";

const captionOf = title => (title ?? "").replace(/^(?:OC|OpenCode)\s*\|\s*/, "").trim();
const sameCaption = (caption, title) => Boolean(caption && title && (caption === title ||
  (caption.endsWith("…") && caption.length >= 17 && title.startsWith(caption.slice(0, -1).trimEnd()))));

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
    this.verifiedResumes = new Map();
    this.explicitClients = new Map(Object.entries(titles).filter(([, item]) =>
      (item?.requiresProbe || validID(item?.retiredReport) || validID(item?.lastReport)) && Number.isFinite(item.created) && Object.hasOwn(item, "machine"))
      .map(([address, { machine, pid, clientPid = pid, created, requiresProbe, awaitingLaunch, retiredReport,
        retiredVersion, lastReport, lastVersion }]) =>
        [address, { machine, clientPid, created, requiresProbe, awaitingLaunch, retiredReport, retiredVersion,
          lastReport: validID(lastReport) ? lastReport : undefined, lastVersion }]));
    this.presentationNames = new Map();
    this.presentationHints = new Map(Object.entries(titles).filter(([, item]) =>
      item?.presentationOnly && validID(item.sessionID) && Number.isFinite(item.pid) && Number.isFinite(item.created)
      && typeof item.caption === "string" && Object.hasOwn(item, "machine")));
    this.bindings = new Map(Object.entries(titles).filter(([, item]) =>
      !item?.presentationOnly && validID(item?.sessionID) && Number.isFinite(item.pid) && Number.isFinite(item.created))
      .map(([address, item]) => [address, { id: item.sessionID, pid: item.pid,
        created: item.created, caption: captionOf(item.caption), proofCaption: item.proofCaption ?? item.caption, title: item.title }]));
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
    const liveTabs = new Set([...sp.values()].flatMap(s => (s.tabs ?? []).map(t => t.id)));
    const missingHere = new Set([...ps.values()].filter(p => p.terminal && liveTabs.has(p.tab) && !ts.has(p.terminal)).map(p => p.terminal));
    const list = attachedAgents(agents(), [...ps.values()], ts, [...sp.values()]).filter(a => ts.has(a.address)
      && !ts.get(a.address).parent && isOpenCode(a, ts.get(a.address)));
    this.prune(new Set(list.map(a => a.address)), ts, missingHere);
    for (const a of list) {
      if (!sp.has(a.space)) continue;
      const machine = sp.get(a.space).machine ?? null;
      if (!groups.has(machine)) continue;
      const terminal = ts.get(a.address);
      this.checkPresentation(a, terminal, machine);
      const launch = terminalLaunch(terminal), probe = serviceProbe(terminal, machine, launch);
      const sessionID = this.boundSessionID(a, terminal, true, machine);
      const previousBinding = this.bindings.get(a.address);
      // On a reload a saved title can remember this launch, but it does not
      // carry a live service check. The same explicit ID can be reverified.
      const recheckingLaunch = probe && this.verifiedResumes.get(a.address) !== probe.key
        && previousBinding?.id === launch.id;
      // A retained idle ID is a precise recheck target, not a title search.
      // Use only the same process/terminal with its original caption and no
      // conflicting native records, even across a module reload.
      const retained = !launch?.id && probe && a.status === "idle" && previousBinding?.proofCaption === a.title
        && previousBinding?.pid === (terminal?.foreground?.pid ?? terminal?.pid)
        && previousBinding?.created === terminal?.created_ms && !Object.keys(terminal?.records ?? {}).length;
      const idToVerify = this.observedSessionID(a, terminal) ?? sessionID
        ?? (retained ? previousBinding.id : !previousBinding || recheckingLaunch ? launch?.id : null);
      const previous = this.good[a.address];
      const presentationKey = this.presentationKey(a, terminal, machine);
      const hint = this.presentationHints.get(a.address);
      const client = this.explicitClient(a);
      const blockedClient = (client?.requiresProbe && !probe)
        || (client?.retiredReport && !idToVerify && !sessionID);
      const sameProcess = previous?.presentationKey === presentationKey;
      const reusable = sameProcess && (sessionID ? previous?.sessionID === sessionID : previous?.title === a.title);
      const target = {
        address: a.address,
        title: a.title,
        cwd: ps.get(a.pane)?.cwd,
        sessionID,
        resumeProbe: probe ? { server: probe.server, id: idToVerify ?? undefined } : undefined,
        serviceKey: probe?.key,
        foreignService: Boolean(blockedClient),
        presentationKey,
        presentationID: !sessionID && !idToVerify && !blockedClient ? hint?.sessionID : undefined,
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
    const previousDesired = this.desiredKeys;
    this.desiredKeys = new Map([...groups].map(([machine, targets]) => {
      const signature = JSON.stringify(targets.map(({ address, title, cwd, sessionID, resumeProbe, serviceKey, foreignService, presentationKey, presentationID }) =>
        [address, title, cwd, sessionID, resumeProbe, serviceKey, foreignService, presentationKey, presentationID]));
      const previous = previousDesired.get(machine);
      // Object identity is the generation: A → B → A must retire the first
      // request for A even if B never got a chance to start its own request.
      return [machine, previous?.signature === signature ? previous : { signature }];
    }));
    for (const machine of this.background.keys()) if (!groups.has(machine)) {
      this.background.delete(machine);
      cx.notify();
    }
    for (const [machine, targets] of groups) {
      const desired = this.desiredKeys.get(machine), { signature } = desired;
      const changed = desired !== this.targetKeys.get(machine);
      if (this.busy.has(machine) && this.requests.get(machine)?.desired === desired) continue;
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
        this.targetKeys.set(machine, desired);
        this.bridgeKeys.delete(machine);
        this.retryAt.delete(machine);
        this.failures.delete(machine);
      }
      const request = { signature, desired };
      const current = () => !this.closed && this.requests.get(machine) === request
        && this.desiredKeys.get(machine) === desired;
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
          const args = [JSON.stringify(targets.map(({ serviceKey, presentationKey, ...target }) => target)), JSON.stringify(this.bridgeKeys.get(machine) ?? null),
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
            presentationKey: target.presentationKey,
          };
          if (current()) for (const target of targets) {
            if (!target.resumeProbe) continue;
            const info = next[target.address];
            if (info.resumeVerified && (!target.resumeProbe.id || info.sessionID === target.resumeProbe.id)) {
              this.verifiedResumes.set(target.address, target.serviceKey);
            } else if (info.resumeVerified === false && this.verifiedResumes.get(target.address) === target.serviceKey) {
              this.verifiedResumes.delete(target.address);
            }
          }
          if (current()) for (const target of targets) {
            const info = next[target.address];
            if (!target.sessionID && !target.resumeProbe?.id && target.presentationKey
              && !info.excluded && info.resumeVerified !== false && validID(info.sessionID)
              && typeof info.sessionTitle === "string") {
              const terminal = ts.get(target.address);
              this.presentationHints.set(target.address, {
                sessionID: info.sessionID, caption: target.title ?? "", machine,
                pid: terminal.foreground?.pid ?? terminal.pid, created: terminal.created_ms, presentationOnly: true,
                requiresProbe: Boolean(target.resumeProbe),
              });
            }
          }
        } catch {
          if (current()) this.bridgeStates.delete(machine);
          for (const target of targets) next[target.address] = { title: target.title, sessionID: target.sessionID,
            presentationKey: target.presentationKey, error: "OpenCode task API is unavailable" };
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
    this.verifiedResumes.clear();
    this.explicitClients.clear();
    this.presentationHints.clear();
    this.presentationNames.clear();
    // In-flight bounded exec/subscriptions expire themselves within 27s;
    // closed prevents publication or reconnect, including after hot reload.
  }

  stale(info, good) {
    return Boolean(info.error) || Date.now() - good.checkedAt > (good.eventBridge ? 45000 : 20000);
  }

  presentationKey(agent, terminal, machine) {
    const pid = terminal?.foreground?.pid ?? terminal?.pid, created = terminal?.created_ms;
    if (!terminal || !isOpenCode(agent, terminal) || !Number.isFinite(pid) || !Number.isFinite(created)) return null;
    return JSON.stringify([machine ?? null, pid, created, terminalLaunch(terminal)]);
  }

  rememberClient(agent, terminal, machine, launch) {
    const address = agent.address, origin = machine ?? null;
    const created = terminal.created_ms, foregroundPID = terminal.foreground?.pid ?? terminal.pid;
    let prior = this.explicitClients.get(address);
    const newTerminal = prior && (prior.machine !== origin || (Number.isFinite(created) && prior.created !== created));
    const newProcess = prior && Number.isFinite(foregroundPID) && Number.isFinite(prior.clientPid)
      && foregroundPID !== prior.clientPid;
    // The current snapshot belongs to the replacement; its report might
    // already be fresh. Retire only an ID observed before the PID changed.
    const retiredReport = !newTerminal && (newProcess
      ? prior.lastReport ?? prior.retiredReport : prior?.retiredReport) || undefined;
    const retiredVersion = !newTerminal && (newProcess
      ? (prior.lastReport ? prior.lastVersion : prior.retiredVersion) : prior?.retiredVersion) || undefined;
    const priorRequiresProbe = prior?.requiresProbe;
    if (newTerminal || newProcess) {
      this.explicitClients.delete(address);
      prior = null;
    }
    if (launch && launch.server !== null) {
      this.explicitClients.set(address, { machine: origin, clientPid: foregroundPID ?? prior?.clientPid ?? null,
        created, requiresProbe: true, retiredReport, retiredVersion, lastReport: prior?.lastReport, lastVersion: prior?.lastVersion });
      return true;
    }
    // A PID change invalidates the prior binding, but the terminal may still
    // show its old report. Wait until the new process exposes its launch.
    if (!newTerminal && newProcess && !launch && priorRequiresProbe) {
      this.explicitClients.set(address, { machine: origin, clientPid: foregroundPID, created,
        requiresProbe: true, awaitingLaunch: true, retiredReport, retiredVersion });
      return true;
    }
    if ((prior?.awaitingLaunch && launch) || (newProcess && retiredReport)) {
      if (retiredReport) this.explicitClients.set(address, { machine: origin, clientPid: foregroundPID, created,
        requiresProbe: false, retiredReport, retiredVersion });
      else this.explicitClients.delete(address);
      return false;
    }
    return Boolean(prior?.requiresProbe);
  }

  explicitClient(agent) {
    return this.explicitClients.get(agent.address);
  }

  observedSessionID(agent, terminal) {
    const observed = terminalSessionID(terminal);
    const client = this.explicitClient(agent);
    if (!observed || observed !== client?.retiredReport) return observed;
    const version = terminalReportVersion(terminal, observed);
    return terminalReportAdvanced(client.retiredVersion, version) ? observed : null;
  }

  checkPresentation(agent, terminal, machine) {
    const key = this.presentationKey(agent, terminal, machine);
    const hint = this.presentationHints.get(agent.address);
    if (hint && (!key || hint.machine !== (machine ?? null) || hint.caption !== (agent.title ?? "")
      || hint.pid !== (terminal.foreground?.pid ?? terminal.pid) || hint.created !== terminal.created_ms)) {
      this.presentationHints.delete(agent.address);
    }
    for (const state of [this.results, this.good]) {
      const info = state[agent.address];
      if (info?.presentationKey && info.presentationKey !== key) delete state[agent.address];
    }
    const name = this.presentationNames.get(agent.address);
    if (name?.presentationKey !== key) this.presentationNames.delete(agent.address);
  }

  boundSessionID(agent, terminal, forLookup = false, machine = null) {
    const address = agent.address;
    if (!terminal || !isOpenCode(agent, terminal)) return null;
    const pid = terminal.foreground?.pid ?? terminal.pid ?? null;
    const created = terminal.created_ms ?? null;
    const caption = captionOf(agent.title);
    const launch = terminalLaunch(terminal), key = serviceProbe(terminal, machine, launch)?.key;
    const needsServiceCheck = this.rememberClient(agent, terminal, machine, launch);
    let old = this.bindings.get(address);
    if (old && (old.pid !== pid || old.created !== created || (old.serviceKey && old.serviceKey !== key))) {
      this.bindings.delete(address);
      old = null;
    }
    if (this.verifiedResumes.get(address) !== key) this.verifiedResumes.delete(address);
    const verified = key && this.verifiedResumes.get(address) === key;
    // A keyed report can identify a session on a different service. Resolve
    // service scope before consulting records, the launch, or a saved binding.
    if (needsServiceCheck && !verified) return null;
    const observed = this.observedSessionID(agent, terminal);
    let client = this.explicitClient(agent);
    // Even the default service can leave a keyed report on a reused terminal.
    // Store the outgoing process and report version before the next snapshot;
    // observing a fresh report is the only way to retire that guard.
    if (observed && Number.isFinite(pid) && Number.isFinite(created)) {
      client ??= { machine: machine ?? null, clientPid: pid, created, requiresProbe: false };
      if (client.clientPid === pid) {
        client.lastReport = observed;
        client.lastVersion = terminalReportVersion(terminal, observed);
        delete client.retiredReport;
        delete client.retiredVersion;
        this.explicitClients.set(address, client);
      }
    }
    // An unselected keyed record is contradictory evidence, not a missing
    // snapshot. Only an explicitly retired predecessor belongs to another PID.
    if (!observed && Object.keys(terminal.records ?? {}).some(id => id !== client?.retiredReport)) {
      this.bindings.delete(address);
      return null;
    }
    const current = old && this.session(agent, old.id)?.sessionTitle;
    const sameSessionCaption = old && (caption === old.caption || sameCaption(caption, current));
    const resume = launch?.id && (!old || ((verified || client?.retiredReport)
      && old.id === launch.id && !old.observed && sameSessionCaption));
    const id = observed || (resume ? launch.id : null);
    if (id) {
      this.presentationHints.delete(address);
      this.bindings.set(address, { id, pid, created, caption, proofCaption: agent.title ?? "", title: old?.id === id ? old.title : undefined,
        observed: Boolean(observed), serviceKey: key });
      return id;
    }
    // An unkeyed working report may be the first turn of a different session.
    // Keep the last proven binding only while idle, never to merge that turn.
    if (!old || agent.status === "working" || agent.status === "blocked") return null;
    if (sameSessionCaption) {
      old.caption = caption;
      return old.id;
    }
    // Allow the server rename to catch up without ever painting a stale
    // title over a terminal that may have switched to another idle session.
    return forLookup ? old.id : null;
  }

  resumeProof(agent, terminal, machine, id) {
    const launch = terminalLaunch(terminal), binding = this.bindings.get(agent.address);
    if (!launch || launch.id !== id || binding?.id !== id || binding.observed || launch.server === false) return null;
    if (!launch.server) return this.results[agent.address]?.sessionID === id && !this.results[agent.address]?.error;
    const probe = serviceProbe(terminal, machine, launch);
    return Boolean(probe && this.verifiedResumes.get(agent.address) === probe.key);
  }

  session(agent, sessionID) {
    const info = this.results[agent.address];
    const known = info?.sessionID && (Object.hasOwn(info, "sessionTitle") || !info.error) ? info : this.good[agent.address];
    if (!sessionID) return null;
    if (known?.sessionID === sessionID) return known;
    const binding = this.bindings.get(agent.address);
    return binding?.id === sessionID && binding.title ? { sessionID, sessionTitle: binding.title } : null;
  }

  presentation(agent) {
    const name = this.presentationNames.get(agent.address);
    return name?.title === agent.title ? name : null;
  }

  presentationHint(agent) {
    return this.presentationHints.get(agent.address);
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
      const lastName = this.presentationNames.get(address);
      if (info.matchMissing || info.excluded || info.resumeVerified === false
        || (lastName && (info.presentationKey !== lastName.presentationKey || info.title !== lastName.title
          || (info.sessionID && info.sessionID !== lastName.sessionID)))) {
        this.presentationNames.delete(address);
      }
      if (!info.matchMissing && !info.excluded && info.resumeVerified !== false
        && info.presentationKey && validID(info.sessionID) && typeof info.sessionTitle === "string") {
        const { presentationKey, sessionID, sessionTitle, title } = info;
        this.presentationNames.set(address, { presentationKey, sessionID, sessionTitle, title });
      }
      const old = this.good[address];
      const same = old && !info.matchMissing && info.resumeVerified !== false && old.presentationKey === info.presentationKey && (old.sessionID && info.sessionID
        ? old.sessionID === info.sessionID : old.title === info.title);
      if (!same) {
        delete this.good[address];
        delete this.zeroes[address];
      }
      if (info.error) {
        delete this.zeroes[address];
        continue;
      }
      if (info.excluded) {
        delete this.good[address];
        continue;
      }
      const good = { ...info, checkedAt: Date.now(), pendingZero: [] };
      if (same && old.meta && info.meta) {
        if ((info.meta.durationError || !info.meta.durationReady) && old.meta.durationReady) {
          good.meta = { ...good.meta, durationMs: old.meta.durationMs, durationReady: true,
            durationAt: 0, durationRunning: false, durationStale: true };
        }
        if (info.meta.contextError && old.meta.contextTokens != null) {
          good.meta = { ...good.meta, contextTokens: old.meta.contextTokens, contextLimit: old.meta.contextLimit,
            contextModelID: old.meta.contextModelID, contextProviderID: old.meta.contextProviderID };
        }
        if (info.meta.contextError && !info.meta.modelID && old.meta.modelID) {
          good.meta = { ...good.meta, modelID: old.meta.modelID, providerID: old.meta.providerID,
            variant: old.meta.variant, modelFromHistory: old.meta.modelFromHistory, modelStale: true };
        }
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

  prune(addresses, liveTerminals = addresses, missingHere = new Set()) {
    // A pane can outlast a core snapshot. Keep the outgoing identity solely
    // for checking the returning process; display and service proof expire.
    for (const address of this.bindings.keys()) if (!addresses.has(address) && !missingHere.has(address)) this.bindings.delete(address);
    for (const address of this.verifiedResumes.keys()) if (!addresses.has(address)) this.verifiedResumes.delete(address);
    for (const address of this.explicitClients.keys()) if (!liveTerminals.has(address) && !missingHere.has(address)) this.explicitClients.delete(address);
    for (const address of this.presentationHints.keys()) if (!addresses.has(address)) this.presentationHints.delete(address);
    for (const address of this.presentationNames.keys()) if (!addresses.has(address)) this.presentationNames.delete(address);
    for (const address of Object.keys(this.results)) if (!addresses.has(address)) {
      delete this.results[address];
      delete this.good[address];
      delete this.zeroes[address];
    }
  }
}
