import { createHash } from "crypto";

export function verifiedTitle(terminal, pane, value) {
  if (!value || !terminal) return false;
  const report = terminal.report ?? (typeof terminal.status === "object" ? terminal.status : null);
  const current = terminal.records?.[value.sessionID];
  if (report?.app ? report.app !== "opencode" : pane.program?.key !== "opencode") return false;
  const matches = Object.values(terminal.records ?? {}).filter(r =>
    r?.app === "opencode" && (!report || (r.state === report.state && r.title === report.title)));
  if (Object.keys(terminal.records ?? {}).length && (matches.length !== 1 || matches[0] !== current)) return false;
  if (value.pid != null && value.pid !== (terminal.foreground?.pid ?? terminal.pid)) return false;
  if (value.created != null && value.created !== terminal.created_ms) return false;
  if (matches.length && !value.presentationOnly) return true;
  return value.pid === (terminal.foreground?.pid ?? terminal.pid ?? null)
    && value.created === (terminal.created_ms ?? null) && value.caption === pane.title;
}

// A display fallback is insufficient to suppress a real tab. The producer
// issues this pair only for two bound IDs. Validate its process and current
// evidence again, even between publication and the next sidebar render.
function mirrorIdentity(terminal, pane, v) {
  if (!terminal || !v || v.presentationOnly || !/^ses[A-Za-z0-9_-]+$/.test(v.sessionID ?? "")
    || !Number.isFinite(v.pid) || !Number.isFinite(v.created) || v.pid !== (terminal.foreground?.pid ?? terminal.pid)
    || v.created !== terminal.created_ms || v.argsKey !== createHash("sha256").update(JSON.stringify(terminal.foreground?.argv ?? null)).digest("hex")) return false;
  const report = terminal.report ?? (typeof terminal.status === "object" ? terminal.status : null);
  if (report?.app ? report.app !== "opencode" : pane.program?.key !== "opencode") return false;
  const records = Object.entries(terminal.records ?? {});
  if (records.length) {
    const matches = records.filter(([, r]) => report?.app === "opencode" && r?.app === report.app
      && (report.title || report.state) && r.title === report.title && r.state === report.state);
    return matches.length === 1 && matches[0][0] === v.sessionID && v.retiredReport !== v.sessionID;
  }
  if (pane.title !== v.caption) return false;
  if (v.resumeProof === true) {
    // Validate the explicit ID too; a generic --auto launch is never proof.
    const argv = terminal.foreground?.argv ?? [];
    const args = argv.slice(1, argv.includes("--") ? argv.indexOf("--") : undefined);
    const ids = args.flatMap((arg, i) => arg === "-s" || arg === "--session" ? [args[i + 1]]
      : typeof arg === "string" && arg.startsWith("--session=") ? [arg.slice(10)] : []);
    if (argv[0]?.split(/[\\/]/).at(-1) === "opencode" && ids.length === 1 && ids[0] === v.sessionID) return true;
  }
  const states = [report?.state, pane.status, typeof terminal.status === "string" && terminal.status].filter(Boolean);
  return v.proofCaption === pane.title && states.length > 0 && states.every(s => s === "idle" || s === "done");
}

// Hide a duplicate client only while BOTH ends of the published relation still
// resolve to their native panes and process. Never hide a split or the last client.
export function mirrorTabs(list, ps, ts, titles) {
  const result = new Set();
  for (const p of ps) {
    const v = titles[p.terminal], ref = v?.mirrorOf;
    if (!ref || !mirrorIdentity(ts.get(p.terminal), p, v)) continue;
    if (ref.sourceTab !== p.tab || ref.sourceSpace === ref.space) continue;
    const tab = list.find(s => s.id === ref.sourceSpace)?.tabs.find(t => t.id === p.tab);
    if (tab?.panes !== 1 || ps.filter(other => other.tab === p.tab).length !== 1) continue;
    const owner = ps.find(other => other.tab === ref.tab && other.terminal === ref.terminal);
    const value = titles[ref.terminal];
    if (owner && value && !value.presentationOnly && !v.presentationOnly && value.sessionID === v.sessionID
      && list.some(s => s.id === ref.space && s.tabs.some(t => t.id === ref.tab))
      && mirrorIdentity(ts.get(owner.terminal), owner, value)) result.add(p.tab);
  }
  return result;
}
