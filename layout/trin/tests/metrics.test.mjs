import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const src = fs.readFileSync(new URL("../mods/trin-agents/metrics.js", import.meta.url), "utf8");
const { formatTokens, formatActiveTime, modelLabel, activeTime, WorkingClock } = await import(`data:text/javascript,${encodeURIComponent(src)}`);

test("context tokens use compact units with exact values left to tooltips", () => {
  assert.equal(formatTokens(272000), "272k");
  assert.equal(formatTokens(36774917), "36.8m");
  assert.equal(formatTokens(999999), "1m");
  assert.equal(formatTokens(1200000000), "1.2b");
  assert.equal(formatTokens(0), "0");
  assert.equal(formatTokens(undefined), "—");
});

test("active duration is elapsed work, not status age", () => {
  assert.equal(formatActiveTime(0), "00:00");
  assert.equal(formatActiveTime(34000), "00:34");
  assert.equal(formatActiveTime(272000), "04:32");
  assert.equal(formatActiveTime(3599000), "59:59");
  assert.equal(formatActiveTime(3600000), "1:00:00");
  assert.equal(formatActiveTime(3720000), "1:02:00");
  assert.equal(formatActiveTime(3721000), "1:02:01");
  assert.equal(formatActiveTime(90061000), "25:01:01");
  assert.equal(formatActiveTime(null), "—");
});

test("model labels retain family and version", () => {
  assert.equal(modelLabel("gpt-6.1-sol-fast"), "6.1 Sol Fast");
  assert.equal(modelLabel("claude-opus-5-5"), "Opus 5.5");
  assert.equal(modelLabel(null), "—");
});

test("live clock advances only for ready and fresh data", () => {
  const meta = { durationReady: true, durationMs: 10000, durationRunning: true, durationAt: 1000 };
  assert.equal(activeTime(meta, 5000), 14000);
  assert.equal(activeTime({ ...meta, stale: true }, 5000), 10000);
  assert.equal(activeTime({ ...meta, durationStale: true }, 5000), 10000);
  assert.equal(activeTime({ ...meta, durationReady: false }, 5000), null);
});

const agent = { address: "t1", title: "Task", status: "working" };
const history = { sessionID: "ses_1", durationReady: true, durationMs: 10000, durationRunning: false };

test("working counts every second even with no open assistant step", () => {
  const clock = new WorkingClock();
  for (let second = 0; second <= 5; second++) {
    clock.sync([agent], () => history, second * 1000);
    assert.equal(clock.value("t1"), 10000 + second * 1000);
  }
});

test("stale SSE, timing errors and lower snapshots cannot freeze or rewind working time", () => {
  const clock = new WorkingClock();
  clock.sync([agent], () => history, 0);
  clock.sync([agent], () => ({ ...history, durationMs: 5000, stale: true, durationStale: true }), 2000);
  assert.equal(clock.value("t1"), 12000);
  clock.sync([agent], () => ({ durationError: "unavailable" }), 5000);
  assert.equal(clock.value("t1"), 15000);
  clock.sync([agent], () => null, 10000);
  assert.equal(clock.value("t1"), 20000);
});

test("loading history never stops the locally observed working clock", () => {
  const clock = new WorkingClock();
  clock.sync([agent], () => null, 0);
  assert.equal(clock.value("t1"), 0);
  clock.sync([agent], () => ({ durationReady: false }), 3000);
  assert.equal(clock.value("t1"), 3000);
  clock.sync([agent], () => history, 4000);
  assert.equal(clock.value("t1"), 10000);
  clock.sync([agent], () => history, 5000);
  assert.equal(clock.value("t1"), 11000);
});

test("idle and blocked stop, then working resumes without counting the idle gap", () => {
  const clock = new WorkingClock();
  clock.sync([agent], () => history, 0);
  clock.sync([{ ...agent, status: "idle" }], () => history, 2000);
  assert.equal(clock.value("t1"), 12000);
  clock.sync([{ ...agent, status: "blocked" }], () => history, 100000);
  assert.equal(clock.value("t1"), 12000);
  clock.sync([agent], () => history, 200000);
  clock.sync([agent], () => history, 201000);
  assert.equal(clock.value("t1"), 13000);
});

test("a delayed render counts all elapsed working time, not just timer callbacks", () => {
  const clock = new WorkingClock();
  clock.sync([agent], () => history, 0);
  clock.sync([agent], () => history, 120000);
  assert.equal(clock.value("t1"), 130000);
});

test("new session resets while rename of the same known session preserves time", () => {
  const clock = new WorkingClock();
  clock.sync([agent], () => history, 0);
  clock.sync([{ ...agent, title: "Renamed" }], () => history, 2000);
  assert.equal(clock.value("t1"), 12000);
  clock.sync([agent], () => ({ ...history, sessionID: "ses_2", durationMs: 500 }), 3000);
  assert.equal(clock.value("t1"), 500);
  clock.sync([], () => null, 4000);
  assert.equal(clock.value("t1"), null);
});
