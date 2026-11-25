import { createHash } from "crypto";
// Session identity is scoped to a machine. Titles are presentation only.
export const sessionAddress = (machine, sessionID) => `opencode-session:${encodeURIComponent(machine ?? "")}/${sessionID}`;
export const validID = id => typeof id === "string" && /^ses[A-Za-z0-9_-]+$/.test(id);
export const isOpenCode = (agent, terminal) => agent.app === "opencode" || agent.program?.key === "opencode"
  || (terminal?.report ?? terminal?.status)?.app === "opencode";

export function terminalLaunch(terminal) {
  const argv = terminal?.foreground?.argv;
  if (!Array.isArray(argv) || argv.some(arg => typeof arg !== "string")
    || argv[0]?.split(/[\\/]/).at(-1) !== "opencode") return null;
  const args = argv.slice(1, argv.includes("--") ? argv.indexOf("--") : undefined);
  const values = (names, inline) => args.flatMap((arg, i) => names.includes(arg) ? [args[i + 1]]
    : arg.startsWith(inline) ? [arg.slice(inline.length)] : []);
  const ids = values(["-s", "--session"], "--session=");
  const servers = values(["--server"], "--server=");
  const id = ids.length === 1 && validID(ids[0]) ? ids[0] : null;
  if (args.some(arg => arg === "--standalone" || arg.startsWith("--standalone="))) return { id, server: false };
  // An explicit endpoint is only a candidate. The bridge compares it locally
  // to its registered service; it never requests this supplied address.
  if (servers.length && (servers.length !== 1 || !/^https?:\/\/[a-z0-9.:[\]-]+(?:\/[a-z0-9._~/-]*)?$/i.test(servers[0] ?? ""))) {
    return { id, server: false };
  }
  return { id, server: servers[0] ?? null };
}

export function serviceProbe(terminal, machine, launch = terminalLaunch(terminal)) {
  const pid = terminal?.foreground?.pid ?? terminal?.pid, created = terminal?.created_ms;
  return launch?.server && Number.isFinite(pid) && Number.isFinite(created)
    ? { server: launch.server, key: JSON.stringify([machine ?? null, pid, created, launch.server, launch.id]) }
    : null;
}

export function terminalSessionID(terminal) {
  const status = terminal?.report ?? terminal?.status;
  if (status?.app && status.app !== "opencode") return null;
  const entries = Object.entries(terminal?.records ?? {}).filter(([id, report]) =>
    validID(id) && report?.app === "opencode");
  // Only trust the record selected by the aggregate status; a single record
  // can also be left over from a previous session.
  if (status?.app !== "opencode" || (!status.state && !status.title)) return entries.length === 1 ? entries[0][0] : null;
  const chosen = entries.filter(([, report]) => report.state === status.state && report.title === status.title);
  return chosen.length === 1 ? chosen[0][0] : null;
}

// A different status/title or a later native status timestamp is new evidence
// even when a replacement process reopens the very same session ID.
export function terminalReportVersion(terminal, id) {
  const report = terminal?.records?.[id];
  if (!report) return null;
  return [report.state ?? null, report.title ?? null, Number.isFinite(terminal.status_ms) ? terminal.status_ms : null];
}

export function terminalReportAdvanced(previous, current) {
  if (!Array.isArray(previous) || !current) return false;
  const before = previous[2], after = current[2];
  if (Number.isFinite(before) && Number.isFinite(after)) {
    if (after < before) return false;
    if (after > before) return true;
  }
  return current[0] !== previous[0] || current[1] !== previous[1];
}

// An agent status can disappear before the pane/terminal does. Keep using the
// physical attachment while those two native snapshots agree.
export function attachedAgents(reported, ps, ts, sp) {
  const byPane = new Map(ps.map(p => [p.pane, p]));
  const tabSpaces = new Map(sp.flatMap(s => (s.tabs ?? []).map(t => [t.id, s])));
  const result = reported.filter(a => {
    const pane = byPane.get(a.pane), space = tabSpaces.get(a.tab);
    return (!pane || (pane.tab === a.tab && pane.terminal === a.address)) && (!space || space.id === a.space);
  });
  const present = new Set(result.map(a => a.address));
  for (const pane of ps) {
    const address = pane.terminal, terminal = ts.get(address), space = tabSpaces.get(pane.tab);
    if (!terminal || !space || present.has(address) || !isOpenCode(pane, terminal)) continue;
    present.add(address);
    result.push({ ...pane, address, space: space.id, space_name: space.name,
      status: pane.status ?? terminal.report?.state ?? "idle" });
  }
  return result;
}

// Only a comparison token crosses the mod boundary, never process arguments.
export const processArgsKey = terminal => createHash("sha256").update(JSON.stringify(terminal.foreground?.argv ?? null)).digest("hex");
