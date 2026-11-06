import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const metrics = fs.readFileSync(new URL("../mods/trin-agents/metrics.js", import.meta.url), "utf8");
const metricsURL = `data:text/javascript,${encodeURIComponent(metrics)}`;
const src = fs.readFileSync(new URL("../mods/trin-agents/tooltips.js", import.meta.url), "utf8")
  .replace('"./metrics.js"', JSON.stringify(metricsURL));
const { contextTooltip, subagentsTooltip, shellsTooltip } = await import(`data:text/javascript,${encodeURIComponent(src)}`);

test("context tooltip separates current usage, capacity and session totals", () => {
  const tip = contextTooltip({ contextTokens: 167000, contextLimit: 1000000, totalTokens: 1234567,
    usage: { input: 100, cacheRead: 800, cacheWrite: 100, output: 50, reasoning: 10 } });
  assert.match(tip, /^Context: 167,000 tokens\nLimit: 1,000,000 tokens \(16\.7% used\)/);
  assert.match(tip, /Session total: 1,234,567 tokens/);
  assert.match(tip, /Input \(uncached\): 100 tokens/);
  assert.match(tip, /Cache: 900 tokens \(80% hit\)\n  Read: 800 tokens\n  Write: 100 tokens/);
  assert.match(tip, /Output: 50 tokens\nReasoning: 10 tokens/);
});

test("missing limits and zero input never invent capacity or cache hit rate", () => {
  const tip = contextTooltip({ contextTokens: 0, contextLimit: 0, totalTokens: 0,
    usage: { input: 0, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 0 } });
  assert.match(tip, /Context: 0 tokens\nLimit: unavailable/);
  assert.match(tip, /Cache: 0 tokens\n/);
  assert.doesNotMatch(tip, /NaN|Infinity|% hit/);
  assert.match(contextTooltip(null), /Session total: unavailable/);
});

test("unavailable current context does not hide session totals, and stale stats are labelled", () => {
  const tip = contextTooltip({ contextError: "Context unavailable", totalTokens: 999, stale: true });
  assert.match(tip, /Session total: 999 tokens/);
  assert.match(tip, /Context unavailable/);
  assert.match(tip, /Last known statistics/);
});

test("context measured with another model explicitly names that model", () => {
  const tip = contextTooltip({ modelID: "new", providerID: "openai", contextModelID: "old", contextProviderID: "openai" });
  assert.match(tip, /Context model: openai\/old/);
});

test("subagents tooltip includes direct/nested counts, titles, model, effort and totals", () => {
  const tip = subagentsTooltip({ subagents: 3, subagentsDirect: 2, subagentsNested: 1,
    subagentDetails: [{ title: "Task\nname", modelID: "claude-opus-5-5", variant: "high", totalTokens: 12345, nested: true }] });
  assert.match(tip, /Running subagents: 3\nDirect: 2; nested: 1/);
  assert.match(tip, /• Task name \(nested\)\n  Opus 5\.5 @high · session total: 12,345 tokens/);
  assert.match(tip, /2 more running subagents/);
});

test("shell tooltip identifies program, PID, ownership and surviving child commands", () => {
  const tip = shellsTooltip({ shells: 3, shellsOwn: 1, shellsInherited: 2, shellDetails: [
    { program: "python3", pid: 123, inherited: false, ownerActive: true },
    { program: "chrome", inherited: true, ownerTitle: "Completed scene", ownerActive: false },
  ] });
  assert.match(tip, /This session: 1; subagents: 2/);
  assert.match(tip, /python3 · PID 123\n  Owner: this session/);
  assert.match(tip, /Owner: Completed scene \(owner not working\)/);
  assert.match(tip, /1 more running commands/);
});
