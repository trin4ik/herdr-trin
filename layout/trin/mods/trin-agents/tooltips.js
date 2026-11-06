import { modelLabel } from "./metrics.js";

const valid = n => typeof n === "number" && Number.isFinite(n) && n >= 0;
const tokens = n => valid(n) ? `${Math.round(n).toLocaleString("en-US")} tokens` : "unavailable";
const text = value => String(value ?? "").replace(/[\s\u0000-\u001f\u007f]+/g, " ").trim().slice(0, 100);
const percent = (part, whole) => valid(part) && valid(whole) && whole > 0
  ? `${Number((100 * part / whole).toFixed(1))}%` : null;

export function contextTooltip(meta) {
  const used = meta?.contextTokens;
  const limit = valid(meta?.contextLimit) && meta.contextLimit > 0 ? meta.contextLimit : null;
  const fullness = percent(used, limit);
  const lines = [
    `Context: ${tokens(used)}`,
    `Limit: ${tokens(limit)}${fullness ? ` (${fullness} used)` : ""}`,
  ];
  if (meta?.contextModelID && (meta.contextModelID !== meta.modelID || meta.contextProviderID !== meta.providerID)) {
    lines.push(`Context model: ${text(meta.contextProviderID)}/${text(meta.contextModelID)}`);
  }
  lines.push("", `Session total: ${tokens(meta?.totalTokens)}`);
  const u = meta?.usage;
  if (u) {
    const cache = valid(u.cacheRead) && valid(u.cacheWrite) ? u.cacheRead + u.cacheWrite : null;
    const input = valid(u.input) && valid(cache) ? u.input + cache : null;
    const hit = percent(u.cacheRead, input);
    lines.push(`Input (uncached): ${tokens(u.input)}`,
      `Cache: ${tokens(cache)}${hit ? ` (${hit} hit)` : ""}`,
      `  Read: ${tokens(u.cacheRead)}`,
      `  Write: ${tokens(u.cacheWrite)}`,
      `Output: ${tokens(u.output)}`);
    if (valid(u.reasoning) && u.reasoning > 0) lines.push(`Reasoning: ${tokens(u.reasoning)}`);
    lines.push("Session totals include all models and cache traffic.");
  }
  if (meta?.contextError) lines.push("", text(meta.contextError));
  if (meta?.stale) lines.push("", "Last known statistics; reconnecting.");
  return lines.join("\n");
}

export function subagentsTooltip(info) {
  const details = info.subagentDetails ?? [];
  const lines = [`Running subagents: ${info.subagents}`];
  if (info.subagentsDirect != null) lines.push(`Direct: ${info.subagentsDirect}; nested: ${info.subagentsNested ?? 0}`);
  for (const child of details) {
    lines.push("", `• ${text(child.title) || "Untitled session"}${child.nested ? " (nested)" : ""}`);
    const model = child.modelID ? text(modelLabel(child.modelID)) + (child.variant ? ` @${text(child.variant)}` : "") : "Model unavailable";
    lines.push(`  ${model} · session total: ${tokens(child.totalTokens)}`);
  }
  if (info.subagents > details.length) lines.push(`\n${info.subagents - details.length} more running subagents; details not shown.`);
  return lines.join("\n");
}

export function shellsTooltip(info) {
  const details = info.shellDetails ?? [];
  const lines = [`Background shell commands: ${info.shells} running`,
    `This session: ${info.shellsOwn ?? info.shells}; subagents: ${info.shellsInherited ?? 0}`];
  for (const shell of details) {
    lines.push("", `• ${text(shell.program) || "shell"}${Number.isInteger(shell.pid) && shell.pid > 0 ? ` · PID ${shell.pid}` : ""}`);
    const owner = shell.inherited ? text(shell.ownerTitle) || "Untitled subagent" : "this session";
    lines.push(`  Owner: ${owner}${shell.ownerActive === false ? " (owner not working)" : ""}`);
  }
  if (info.shells > details.length) lines.push(`\n${info.shells - details.length} more running commands; details not shown.`);
  return lines.join("\n");
}
