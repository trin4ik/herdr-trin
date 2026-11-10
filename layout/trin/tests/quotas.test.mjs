import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";
// Tests must work in the public checkout without the private design gallery.
import { subscriptions as fixtureSubscriptions, DEMO_NOW as fixtureNow } from "./fixtures/quota-demo.js";
const snapshot = { generatedAt: fixtureNow, subscriptions: fixtureSubscriptions };

const read = file => fs.readFileSync(new URL(`../mods/trin-quotas/${file}`, import.meta.url), "utf8");
const demo = await import("./fixtures/quota-demo.js");
const metrics = await import(`data:text/javascript,${encodeURIComponent(read("metrics.js"))}`);
const { subscriptions, DEMO_NOW } = demo;

function native() {
  const state = { childNodes: [], attrs: {}, handlers: {} };
  const el = new Proxy(state, { get(target, key) {
    if (key in target) return target[key];
    return (...args) => {
      if (key === "child") state.childNodes.push(args[0]);
      else if (key === "trigger" || key === "content") { state.childNodes.push(args[0]); state.attrs[key] = args; }
      else if (key === "children") state.childNodes.push(...args[0]);
      else if (key === "when") { if (args[0]) args[1](el); }
      else if (key === "on_click") state.handlers.click = args[0];
      else if (key === "transition") (state.attrs.transitions ??= []).push(args);
      else state.attrs[key] = args;
      return el;
    };
  } });
  return el;
}
function nodes(el) {
  if (!el || typeof el !== "object") return [];
  return [el, ...el.childNodes.flatMap(nodes)];
}
const byID = (root, id) => nodes(root).find(el => el.attrs.id?.[0] === id);
const text = el => !el || typeof el !== "object" ? String(el ?? "") : el.childNodes.map(text).join("");
function fixture(width = 268, viewportHeight, colors = {}) {
  const timers = [];
  const cx = { notifications: 0, notify() { this.notifications++; }, timer: { after(ms, callback) { timers.push({ ms, callback }); } } };
  const context = { View: class {}, div: native, svg: native, h_flex: native, v_flex: native,
    Popover: { new: id => native().id(id) },
    theme: () => colors, layout: () => ({ id: "sidebar", size: { px: width } }),
    QUOTA_URL: "http://127.0.0.1:8098/v1/quotas", ...metrics };
  if (viewportHeight != null) context.window = { viewport_size: () => ({ width: 800, height: viewportHeight }) };
  const source = read("main.js").replace(/^import .*;\n/gm, "").replace("export default class", "class");
  vm.runInNewContext(read("details.js").replace(/^import .*;\n/gm, "").replaceAll("export function", "function")
    + "\nthis.detailHelpers = { detailsHeight, balanceLabel };", context);
  vm.runInNewContext(source + "\nthis.Panel = Quotas;", context);
  const panel = new context.Panel();
  Object.assign(panel, { expanded: null, revealed: null, disclosureVersion: 0, closed: false });
  panel.monitor = { subscriptions, error: null, generatedAt: DEMO_NOW, fetchedAt: null, close() {} };
  return { panel, cx, timers, detailHelpers: context.detailHelpers };
}

test("native demo preserves base windows/resets with one explicit red palette sample", () => {
  for (const [i, s] of subscriptions.entries()) {
    assert.equal(s.id, snapshot.subscriptions[i].id);
    assert.deepEqual(s.windows.map(({ id, usedPercent, resetsAt }) => ({ id, usedPercent, resetsAt })), snapshot.subscriptions[i].windows.map(({ id, usedPercent, resetsAt }) => ({ id, usedPercent: s.id === "kimi-personal" && id === "five-hour" ? 94 : usedPercent, resetsAt })));
  }
});
test("unknown usage is never zero or a full allowance", () => {
  for (const usedPercent of [null, undefined, NaN, -1, 101, "42"]) {
    assert.equal(metrics.remaining({ usedPercent }), null);
    assert.equal(metrics.usage({ usedPercent }), null);
  }
  assert.equal(metrics.remaining({ usedPercent: 100 }), 0);
  assert.equal(metrics.remaining({ usedPercent: 0 }), 100);
});
test("compact demo reset labels preserve hours within a day", () => {
  assert.equal(metrics.resetLabel(subscriptions[0].windows[0].resetsAt, DEMO_NOW), "↻ 2д 4ч");
  assert.equal(metrics.resetLabel(subscriptions[1].windows[0].resetsAt, DEMO_NOW), "↻ 2ч 18м");
  assert.equal(metrics.resetLabel(null, DEMO_NOW), "↻ —");
  assert.equal(metrics.resetLabel("bad", DEMO_NOW), "↻ —");
  assert.equal(metrics.resetLabel(DEMO_NOW, DEMO_NOW), "↻ ждём");
});
test("absolute dates use Moscow time with day/month/year rollover and explicit input offsets", () => {
  for (const [input, expected] of [
    ["2026-11-04T10:00:00Z", "04.11.2026, 13:00 МСК"],
    ["2026-11-04T13:00:00+03:00", "04.11.2026, 13:00 МСК"],
    ["2026-11-04T05:00:00-05:00", "04.11.2026, 13:00 МСК"],
    ["2026-11-04T22:15:00Z", "05.11.2026, 01:15 МСК"],
    ["2026-01-31T23:30:00Z", "01.02.2026, 02:30 МСК"],
    ["2026-12-31T21:00:00Z", "01.01.2027, 00:00 МСК"],
    ["2026-07-01T10:00:00Z", "01.07.2026, 13:00 МСК"],
  ]) assert.equal(metrics.updatedLabel(input), expected);
});
test("detail dates use Moscow time and wide/rail rows no longer have duplicate tooltips", () => {
  for (const width of [268, 44]) {
    const { panel, cx, timers } = fixture(width), base = subscriptions[0];
    const at = "2026-12-31T22:15:00Z";
    const s = { ...base, observedAt: at, lastAttemptAt: at, nextAttemptAt: at,
      error: { message: "Недоступно" }, windows: [{ ...base.windows[0], startedAt: at, resetsAt: at }] };
    panel.monitor.subscriptions = [s];
    panel.monitor.fetchedAt = at;
    panel.monitor.error = { message: "HTTP 503", at };
    panel.setOpen(s.id, true, cx); timers[0].callback(cx);
    const root = panel.render(), card = byID(root, `quota-details-${s.id}`);
    assert.equal(text(card).split("01.01.2027 · 01:15 МСК").length - 1, 4);
    assert.equal(byID(root, `quota-${s.id}`).attrs.tooltip, undefined);
    const tooltip = metrics.quotaTooltip(s, at, panel.monitor);
    assert.equal(tooltip.split("01.01.2027, 01:15 МСК").length - 1, 5);
    assert.doesNotMatch(text(card) + tooltip, /UTC/);
    assert.equal(s.windows[0].resetsAt, at);
  }
});
test("configured primary window is stable, with a fallback for missing configuration", () => {
  const s = subscriptions[2];
  assert.equal(metrics.primaryWindow(s).id, "five-hour");
  assert.equal(metrics.primaryWindow({ ...s, primaryWindowId: "missing" }).id, "five-hour");
  assert.equal(metrics.primaryWindow({ windows: [] }), undefined);
});
test("row and meter show primary consumption without a duplicate subscription tooltip", () => {
  const { panel } = fixture();
  const root = panel.render();
  assert.equal(text(byID(root, "quota-ollama-personal-usage")), "18%");
  assert.ok(nodes(byID(root, "quota-ollama-personal")).some(el => el.attrs.w?.[0] === "18%"));
  assert.equal(byID(root, "quota-ollama-personal").attrs.tooltip, undefined);
  const tooltip = metrics.quotaTooltip(subscriptions.find(s => s.id === "ollama-personal"), DEMO_NOW);
  assert.ok(tooltip.includes("В строке: 5 часов · расход 18% · осталось 82%"));
  assert.ok(tooltip.includes("Неделя: расход 36% · осталось 64%"));
  assert.ok(tooltip.includes("Месяц: расход 24% · осталось 76% · сброс"));
  assert.equal(text(byID(root, "quota-claude-personal-usage")), "72%");
});
test("providers open details without window selectors or changing the configured primary", () => {
  const { panel } = fixture();
  const root = panel.render();
  assert.equal(nodes(root).some(el => Boolean(el.handlers.click)), false);
  assert.equal(panel.selected, undefined);
  assert.equal(byID(root, "quota-ollama-personal-monthly"), undefined);
  assert.equal(byID(root, "quota-view-mode"), undefined);
});
test("providers keep one 28px line and thin meters; identity/reset live in the disclosure, not a tooltip", () => {
  const { panel } = fixture();
  const root = panel.render();
  for (const s of subscriptions) {
    const row = byID(root, `quota-${s.id}`);
    assert.equal(row.attrs.h[0], 28);
    assert.equal(text(row).includes(s.name), false);
    assert.equal(text(row).includes("↻"), false);
    assert.equal(row.attrs.tooltip, undefined);
    assert.ok(row.attrs.accessibility_label[0].includes(s.name));
    assert.ok(nodes(row).some(el => el.attrs.h?.[0] === 6 && el.attrs.rounded?.[0] === 3));
  }
});
test("HTTP error retains last-good values and identifies success and failure timestamps separately", () => {
  const { panel } = fixture();
  const root = panel.render(), s = subscriptions[3];
  assert.equal(text(byID(root, "quota-kimi-personal-usage")), "94%");
  assert.ok(byID(root, "quota-kimi-personal-error"));
  assert.equal(byID(root, "quota-ollama-personal-error"), undefined);
  const tip = metrics.quotaTooltip(s, DEMO_NOW);
  assert.ok(tip.includes("Ошибка HTTP 503: Service Unavailable"));
  assert.ok(tip.includes("Последнее успешное обновление: 04.11.2026, 12:42 МСК\n\nОшибка HTTP 503"));
  assert.equal(metrics.quotaTooltip(subscriptions[2], DEMO_NOW).includes("\n\n"), false);
  assert.ok(tip.includes("Последнее успешное обновление: 04.11.2026, 12:42 МСК"));
  assert.ok(tip.includes("Последняя попытка: 04.11.2026, 12:59 МСК"));
  assert.ok(tip.includes("Месяц: расход 67% · осталось 33%"));
  assert.ok(tip.includes("могут быть устаревшими"));
});
test("usage color boundaries are green below 60, yellow from 60, muted red from 90", () => {
  const theme = { dim: "unknown", ansi2: "green", ansi3: "yellow", ansi1: "red" };
  for (const used of [0, 32, 59]) assert.equal(metrics.quotaColor(theme, used), "green");
  for (const used of [60, 72, 89]) assert.equal(metrics.quotaColor(theme, used), "yellow");
  for (const used of [90, 94, 100]) assert.equal(metrics.quotaColor(theme, used), "red");
  assert.equal(metrics.quotaColor(theme, null), "unknown");
});
test("compact percentages are whole numbers without changing precise tooltip values", () => {
  assert.equal(metrics.compactPercent(47.62), "48%");
  assert.equal(metrics.compactPercent(0.06), "0%");
  assert.equal(metrics.compactPercent(null), "—");
  assert.equal(metrics.percent(47.62), "47.62%");
  assert.equal(metrics.percent(0.06), "0.06%");
});
test("empty and full meters mean zero and exhausted usage, with explicit balanced spacing", () => {
  const { panel } = fixture();
  assert.equal(panel.meter({}, 0).childNodes.length, 0);
  assert.equal(panel.meter({}, 100).childNodes[0].attrs.w[0], "100%");
  const value = metrics.usage({ usedPercent: 59.5 });
  assert.equal(value + metrics.remaining({ usedPercent: 59.5 }), 100);
  const row = byID(panel.render(), "quota-ollama-personal");
  assert.equal(panel.render().attrs.pb[0], 8);
  assert.equal(row.childNodes[0].attrs.mr[0], 12);
  assert.equal(row.attrs.px[0], 16);
  assert.equal(row.childNodes[1].attrs.mr[0], 12);
  assert.ok(row.childNodes[2].attrs.text_right);
  assert.equal(row.childNodes[2].attrs.w, undefined);
});
test("missing timestamps and HTTP failure without history do not invent successful updates or zero usage", () => {
  for (const value of [null, undefined, "invalid"]) assert.equal(metrics.updatedLabel(value), "неизвестно");
  const tip = metrics.quotaTooltip({ ...subscriptions[3], observedAt: null,
    windows: [{ ...subscriptions[3].windows[0], usedPercent: null }], error: { message: "Недоступно" } }, DEMO_NOW);
  assert.ok(tip.includes("Последнее успешное обновление: неизвестно"));
  assert.ok(tip.includes("Данные недоступны; остаток неизвестен"));
  assert.ok(tip.includes("осталось —"));
  assert.equal(tip.includes("осталось 0%"), false);
});
test("rail retains four identifiable providers without demo or wide controls", () => {
  const { panel } = fixture(44);
  const root = panel.render();
  assert.equal(text(root).includes("Демо"), false);
  assert.equal(byID(root, "quota-view-mode"), undefined);
  for (const s of subscriptions) assert.ok(byID(root, `quota-${s.id}`).attrs.accessibility_label[0].includes(s.name));
  assert.equal(text(root).includes("%"), true);
  assert.equal(text(root).includes("Подписки"), false);
  for (const s of subscriptions) {
    const row = byID(root, `quota-${s.id}`);
    assert.equal(row.attrs.h[0], 34);
    const label = byID(row, `quota-${s.id}-usage`);
    assert.equal(text(label), metrics.compactPercent(metrics.usage(metrics.primaryWindow(s))));
    assert.equal(text(label), text(byID(fixture().panel.render(), `quota-${s.id}-usage`)));
    assert.equal(label.attrs.absolute, undefined);
    assert.equal(label.attrs.bg, undefined);
    assert.equal(row.childNodes[1], label);
    assert.equal(row.childNodes[0].attrs.size[0], 22);
    assert.equal(row.attrs.tooltip, undefined);
  }
});
test("rail unknown usage is a dash, not a fabricated zero", () => {
  const { panel } = fixture(44);
  const subscription = { ...subscriptions[0], windows: [] };
  assert.equal(text(byID(panel.row(subscription, {}, true), `quota-${subscription.id}-usage`)), "—");
});
test("rail and wide rows agree for unused, exhausted and fractional quotas including percent sign", () => {
  const { panel } = fixture();
  for (const value of [0, 100, 47.62]) {
    const s = { ...subscriptions[0], windows: [{ ...subscriptions[0].windows[0], usedPercent: value }] };
    for (const narrow of [false, true]) {
      assert.equal(text(byID(panel.row(s, {}, narrow), `quota-${s.id}-usage`)), metrics.compactPercent(value));
    }
  }
});
test("rail has balanced outer breathing room and explicit gaps without changing the wide layout", () => {
  const compact = fixture(44).panel.render(), wide = fixture(268).panel.render();
  assert.equal(compact.attrs.pt[0], 10);
  assert.equal(compact.attrs.pb[0], 9);
  assert.ok(nodes(compact).some(el => el.attrs.gap?.[0] === 8));
  assert.equal(wide.attrs.pt[0], 0);
  assert.equal(wide.attrs.pb[0], 8);
});
test("runtime has one minute polling and no demo dependency or provider collection", () => {
  const source = read("main.js");
  assert.doesNotMatch(source, /demo\.js|DEMO_NOW|ДЕМО|setInterval|setTimeout/);
  assert.doesNotMatch(read("mod.json"), /демо/i);
  assert.match(source, /cx\.timer\.every\(REFRESH_MS/);
  assert.match(read("monitor.js"), /REFRESH_MS = 60000/);
  assert.match(read("transport.js"), /--max-time", "15"/);
  assert.match(read("transport.js"), /http:\/\/127\.0\.0\.1:8098\/v1\/quotas/);
  assert.doesNotMatch(read("transport.js"), /quota\.trin\.box/);
});

test("native provider press opens one bounded animated detail card without changing the summary", () => {
  const { panel, cx, timers } = fixture();
  const root = panel.render(), id = subscriptions[0].id;
  assert.equal(byID(root, `quota-details-${id}`), undefined);
  byID(root, `quota-disclosure-${id}`).attrs.on_open_change[0](true, cx);
  assert.equal(panel.expanded, id);
  assert.equal(panel.revealed, null);
  let card = byID(panel.render(), `quota-details-${id}`);
  assert.equal(card.attrs.h[0], 0);
  assert.equal(timers[0].ms, 16);
  timers[0].callback(cx);
  card = byID(panel.render(), `quota-details-${id}`);
  assert.ok(card.attrs.h[0] > 0 && card.attrs.h[0] <= 600);
  assert.equal(card.attrs.w[0], 344);
  assert.equal(card.attrs.transitions[0][0], "height");
  assert.equal(card.attrs.transitions[0][1].duration, 220);
  assert.equal(text(byID(panel.render(), `quota-${id}-usage`)), text(byID(root, `quota-${id}-usage`)));
});

test("disclosure closes smoothly, repeated clicks and switching invalidate late animation callbacks", () => {
  const { panel, cx, timers } = fixture();
  const first = subscriptions[0].id, second = subscriptions[1].id;
  panel.setOpen(first, true, cx);
  panel.setOpen(second, true, cx);
  timers[0].callback(cx);
  assert.equal(panel.revealed, null);
  timers[1].callback(cx);
  assert.equal(panel.revealed, second);
  assert.equal(byID(panel.render(), `quota-details-${first}`), undefined);
  panel.setOpen(second, false, cx);
  assert.equal(panel.expanded, second);
  assert.equal(panel.revealed, null);
  assert.equal(timers[2].ms, 150);
  assert.equal(byID(panel.render(), `quota-details-${second}`).attrs.transitions[0][1].duration, 150);
  panel.setOpen(second, true, cx);
  timers[2].callback(cx);
  assert.equal(panel.expanded, second);
  timers[3].callback(cx);
  assert.equal(panel.revealed, second);
});

test("details show every scoped window and never invent a missing start or duration", () => {
  const { panel, cx, timers } = fixture(), subscription = subscriptions[3];
  panel.setOpen(subscription.id, true, cx); timers[0].callback(cx);
  const card = byID(panel.render(), `quota-details-${subscription.id}`);
  for (const w of subscription.windows) {
    const section = byID(card, `quota-window-${subscription.id}-${w.id}`);
    assert.ok(text(section).includes(w.label));
    assert.ok(text(section).includes(metrics.percent(metrics.usage(w))));
    assert.ok(text(section).includes("Не сообщено"));
  }
  assert.ok(text(card).includes("Время — МСК"));
});

test("open details use the current monitor snapshot, not a frozen copy or a second poll", () => {
  const { panel, cx, timers } = fixture(), first = subscriptions[0];
  panel.setOpen(first.id, true, cx); timers[0].callback(cx);
  panel.monitor.subscriptions = [{ ...first, windows: first.windows.map(w => ({ ...w, usedPercent: 47.62 })) }];
  const card = byID(panel.render(), `quota-details-${first.id}`);
  assert.ok(text(card).includes("47.62%"));
  assert.equal(text(byID(panel.render(), `quota-${first.id}-usage`)), "48%");
});

test("provider removal and close cannot resurrect a detail card through a pending timer", () => {
  const { panel, cx, timers } = fixture(), id = subscriptions[0].id;
  panel.setOpen(id, true, cx);
  panel.monitor.subscriptions = [];
  panel.render(); timers[0].callback(cx);
  assert.equal(panel.expanded, null);
  assert.equal(panel.revealed, null);
  panel.setOpen(id, true, cx);
  panel.close(); timers[1].callback(cx);
  assert.equal(panel.revealed, null);
});

test("rail disclosure preserves 44px summaries and shares the same detail view", () => {
  const { panel, cx, timers } = fixture(44), id = subscriptions[0].id;
  panel.setOpen(id, true, cx); timers[0].callback(cx);
  const root = panel.render();
  assert.equal(byID(root, `quota-${id}`).attrs.h[0], 34);
  assert.equal(byID(root, `quota-disclosure-${id}`).attrs.anchor[0], "bottom_left");
  assert.equal(byID(root, `quota-details-${id}`).attrs.w[0], 344);
});

test("detail warnings and missing usage remain explicit rather than fabricated zero quotas", () => {
  const { panel, cx, timers } = fixture();
  const first = { ...subscriptions[0], windows: [], error: { message: "Авторизация истекла" } };
  panel.monitor.subscriptions = [first];
  panel.setOpen(first.id, true, cx); timers[0].callback(cx);
  const card = byID(panel.render(), `quota-details-${first.id}`);
  assert.ok(text(card).includes("Данные не подтверждены"));
  assert.ok(text(card).includes("Авторизация истекла"));
  assert.ok(text(card).includes("Сервис пока не сообщил лимиты"));
  assert.equal(text(card).includes("Осталось 0%"), false);
});

test("the visible close control uses the same guarded animated collapse", () => {
  const { panel, cx, timers } = fixture(), id = subscriptions[0].id;
  panel.setOpen(id, true, cx); timers[0].callback(cx);
  let stopped = false;
  cx.stop_propagation = () => { stopped = true; };
  byID(panel.render(), `quota-close-${id}`).handlers.click({}, cx);
  assert.equal(stopped, true);
  assert.equal(panel.revealed, null);
  timers[1].callback(cx);
  assert.equal(panel.expanded, null);
});

test("profile header formats reported tiers and makes missing plan explicit without inference", () => {
  assert.equal(metrics.planLabel("prolite"), "Pro Lite");
  assert.equal(metrics.planLabel("MAX"), "Max");
  assert.equal(metrics.planLabel("pro"), "Pro");
  assert.equal(metrics.planLabel("ultra"), "Ultra");
  assert.equal(metrics.planLabel("Custom 20x"), "Custom 20x");
  assert.equal(metrics.planLabel(null), null);
  assert.equal(metrics.planLabel("  "), null);
  const { panel, cx, timers } = fixture(), first = subscriptions[0];
  panel.monitor.subscriptions = [{ ...first, plan: "prolite" }];
  panel.setOpen(first.id, true, cx); timers[0].callback(cx);
  assert.equal(text(byID(panel.render(), `quota-account-${first.id}`)), "Аккаунт не указан, Pro Lite");
  panel.monitor.subscriptions = [{ ...first, plan: null }];
  assert.equal(text(byID(panel.render(), `quota-account-${first.id}`)), "Аккаунт не указан, тариф не указан");
});

test("all provider profiles top-align logo/close and show account email plus reported plan", () => {
  for (const width of [268, 44]) for (const base of subscriptions) {
    const { panel, cx, timers } = fixture(width);
    const s = { ...base, accountEmail: "account@example.test", plan: "pro" };
    panel.monitor.subscriptions = [s]; panel.setOpen(s.id, true, cx); timers[0].callback(cx);
    const root = panel.render(), profile = byID(root, `quota-profile-${s.id}`), account = byID(root, `quota-account-${s.id}`);
    assert.ok(profile.attrs.items_start); assert.equal(profile.attrs.items_center, undefined);
    assert.equal(profile.attrs.py[0], 12); assert.equal(profile.attrs.h[0], 64);
    assert.equal(profile.childNodes[2].attrs.id[0], `quota-close-${s.id}`);
    assert.equal(text(account), "account@example.test, Pro"); assert.ok(account.childNodes[0].attrs.truncate);
    assert.equal(account.attrs.tooltip[0], "account@example.test, Pro");
    assert.equal(byID(root, `quota-plan-${s.id}`), undefined);
    assert.equal(byID(root, `quota-${s.id}`).attrs.tooltip, undefined);
  }
});
test("missing account/plan are explicit and open profiles react to the current snapshot", () => {
  const { panel, cx, timers } = fixture(), s = { ...subscriptions[0], accountEmail: null, plan: null };
  panel.monitor.subscriptions = [s]; panel.setOpen(s.id, true, cx); timers[0].callback(cx);
  assert.equal(text(byID(panel.render(), `quota-account-${s.id}`)), "Аккаунт не указан, тариф не указан");
  panel.monitor.subscriptions = [{ ...s, accountEmail: "new-account@example.test", plan: "max" }];
  assert.equal(text(byID(panel.render(), `quota-account-${s.id}`)), "new-account@example.test, Max");
});
test("profile uses exactly email comma space tier without a separate tariff label", () => {
  for (const [plan, label] of [["pro", "Pro"], ["max", "Max"], ["prolite", "Pro Lite"], [null, "тариф не указан"]]) {
    const { panel, cx, timers } = fixture(), s = { ...subscriptions[0], plan, accountEmail: "account@example.test" };
    panel.monitor.subscriptions = [s]; panel.setOpen(s.id, true, cx); timers[0].callback(cx);
    const root = panel.render(), summary = byID(root, `quota-account-${s.id}`);
    assert.equal(text(summary), `account@example.test, ${label}`);
    assert.equal(summary.attrs.h[0], 16); assert.equal(summary.attrs.accessibility_label[0], text(summary));
    assert.equal(byID(root, `quota-plan-${s.id}`), undefined);
  }
});
test("profile highlights only the known tier in foreground and keeps email/comma muted", () => {
  for (const plan of ["pro", "max", "prolite", null]) {
    const colors = { muted: "muted-email", foreground: "bright-tier" };
    const { panel, cx, timers } = fixture(268, undefined, colors), s = { ...subscriptions[0], accountEmail: "account@example.test", plan };
    panel.monitor.subscriptions = [s]; panel.setOpen(s.id, true, cx); timers[0].callback(cx);
    const row = byID(panel.render(), `quota-account-${s.id}`), tier = byID(row, `quota-tier-${s.id}`);
    assert.equal(row.attrs.text_color[0], colors.muted);
    assert.equal(tier.attrs.text_color[0], plan ? colors.foreground : colors.muted);
    assert.ok(tier.attrs.flex_none); assert.ok(tier.attrs.truncate); assert.equal(tier.attrs.max_w[0], "50%");
    assert.equal(text(row), `account@example.test, ${metrics.planLabel(plan) || "тариф не указан"}`);
    assert.equal(row.attrs.h[0], 16);
  }
});

test("all providers and every window retain an unshrinkable 4px track even with stale/expired notices", () => {
  for (const subscription of subscriptions) {
    const { panel, cx, timers } = fixture();
    const s = { ...subscription, stale: true, windows: subscription.windows.map(w => ({ ...w, resetsAt: "2020-01-01T00:00:00Z" })) };
    panel.monitor.subscriptions = [s];
    panel.setOpen(s.id, true, cx); timers[0].callback(cx);
    const card = byID(panel.render(), `quota-details-${s.id}`);
    for (const w of s.windows) {
      const section = byID(card, `quota-window-${s.id}-${w.id}`), track = byID(section, `quota-track-${s.id}-${w.id}`);
      assert.ok(section.attrs.flex_none);
      assert.ok(track.attrs.flex_none);
      assert.equal(track.attrs.h[0], 4);
      assert.ok(text(section).includes("Срок прошёл"));
    }
    assert.ok(byID(card, `quota-windows-${s.id}`).attrs.overflow_y_scrollbar);
    assert.ok(card.attrs.h[0] <= 600);
  }
});

test("small viewport caps only the card, never its sections or tracks", () => {
  const { panel, cx, timers } = fixture(44, 400), s = subscriptions[3];
  panel.setOpen(s.id, true, cx); timers[0].callback(cx);
  const card = byID(panel.render(), `quota-details-${s.id}`);
  assert.equal(card.attrs.h[0], 376);
  for (const w of s.windows) {
    assert.ok(byID(card, `quota-window-${s.id}-${w.id}`).attrs.h[0] >= 136);
    assert.equal(byID(card, `quota-track-${s.id}-${w.id}`).attrs.h[0], 4);
  }
});

test("balance padding and line heights match the exact content height budget", () => {
  const { panel, cx, timers, detailHelpers } = fixture();
  const first = { ...subscriptions[0], stale: false, staleAfter: "2050-01-01T00:00:00Z", status: "ok", error: null,
    windows: [{ ...subscriptions[0].windows[0], resetsAt: "2050-01-01T00:00:00Z" }],
    balances: [{ label: "Баланс кредитов", value: 62500, unit: "credits" }] };
  panel.monitor.subscriptions = [first];
  panel.setOpen(first.id, true, cx); timers[0].callback(cx);
  const card = byID(panel.render(), `quota-details-${first.id}`), group = byID(card, `quota-balances-${first.id}`);
  assert.equal(group.attrs.py[0], 12);
  assert.ok(group.attrs.flex_none);
  assert.equal(byID(group, `quota-balance-${first.id}-0`).attrs.h[0], 20);
  assert.ok(byID(group, `quota-balance-${first.id}-0`).attrs.flex_none);
  assert.equal(detailHelpers.detailsHeight(first), 64 + 77 + 2 + 137 + 44);
  assert.equal(card.attrs.h[0], detailHelpers.detailsHeight(first));
  assert.ok(text(group).includes("62\u202f500"));
  assert.equal(detailHelpers.balanceLabel({ value: 0, unit: "USD" }), "0 USD");
  assert.equal(detailHelpers.balanceLabel({ value: 1234.56, unit: "USD" }), "1\u202f234.56 USD");
});

test("zero and unknown consumption both keep the track but do not invent a colored fill", () => {
  for (const value of [0, null]) {
    const { panel, cx, timers } = fixture(), base = subscriptions[2];
    const s = { ...base, windows: [{ ...base.windows[0], usedPercent: value, remainingPercent: null }] };
    panel.monitor.subscriptions = [s];
    panel.setOpen(s.id, true, cx); timers[0].callback(cx);
    const section = byID(panel.render(), `quota-window-${s.id}-${s.windows[0].id}`);
    assert.equal(byID(section, `quota-track-${s.id}-${s.windows[0].id}`).childNodes.length, 0);
    assert.ok(text(section).includes(value === 0 ? "0%" : "—"));
  }
});

// --- OpenCode Console -------------------------------------------------------
const data = await import(`data:text/javascript,${encodeURIComponent(read("data.js"))}`);
const consoleFixture = await import("./fixtures/quota-console.js");

test("the console snapshot keeps counters and a percentage-free refusal", () => {
  const normalized = data.normalizeSnapshot(consoleFixture.consolePayload);
  assert.equal(normalized.subscriptions.length, 1);
  const s = normalized.subscriptions[0];
  assert.equal(s.provider, "opencode");
  assert.equal(s.name, "OpenCode Console");
  assert.equal(s.primaryWindowId, "go_weekly");
  const free = s.windows.find(w => w.id === "provider_refusal");
  assert.equal(free.metric, "counter");
  assert.equal(free.name, "Отказ провайдера");
  assert.equal(free.usedPercent, null);
  const model = s.windows.find(w => w.id === "model_minimax_m3_1_flash_free");
  assert.equal(model.metric, "counter");
  assert.equal(model.used, 121);
  assert.equal(model.scope, "free");
  assert.equal(model.counters.input_tokens, 314460);
  assert.equal(metrics.usage(model), null, "a counter has no percentage");
  const weekly = s.windows.find(w => w.id === "go_weekly");
  assert.equal(weekly.limited, true);
  assert.equal(s.balances[0].label, "Баланс кредитов");
  assert.equal(s.balances[0].value, 12.5);
});

test("an unknown provider keeps its own name and order", () => {
  const normalized = data.normalizeSnapshot({ schema_version: 1, generated_at: consoleFixture.CONSOLE_NOW,
    subscriptions: [{ id: "x", provider: "mystery", status: "ok", quotas: [] }] });
  assert.equal(normalized.subscriptions[0].name, "mystery");
});

test("the console row shows Go allowance while refusal details never invent a percentage", () => {
  const { panel, cx, timers } = fixture();
  const s = consoleFixture.consoleSubscription;
  panel.monitor.subscriptions = [s];
  const row = byID(panel.render(), `quota-${s.id}`);
  assert.equal(text(byID(row, `quota-${s.id}-usage`)), "12%");
  panel.setOpen(s.id, true, cx); timers[0].callback(cx);
  const detail = panel.render();
  const free = byID(detail, `quota-window-${s.id}-provider_refusal`);
  assert.ok(text(free).includes("Отказ провайдера"));
  assert.ok(text(free).includes("HTTP 429"));
  assert.ok(text(free).includes("Повтор"));
  assert.equal(text(free).includes("%"), false);
  assert.equal(byID(free, `quota-track-${s.id}-provider_refusal`), undefined);
  const model = byID(detail, `quota-window-${s.id}-model_minimax_m3_1_flash_free`);
  assert.ok(text(model).includes("121 запрос"), text(model));
  assert.ok(text(model).includes("314k"), text(model));
  assert.ok(text(model).includes("бесплатно"), text(model));
  assert.equal(model.attrs.h[0], 46 + 5 * 16 + 4 * 4 + 1,
    "counter height includes all four gaps between five rows");
  assert.equal(text(model).includes("%"), false, "a counter never shows a percentage");
  const weekly = byID(detail, `quota-window-${s.id}-go_weekly`);
  assert.equal(text(weekly).includes("Без лимита"), false);
  assert.ok(text(weekly).includes("12%"));
});

test("a pay-as-you-go row without any percentage shows the balance, not a dash", () => {
  const { panel } = fixture();
  const s = { ...consoleFixture.consoleSubscription,
    windows: consoleFixture.consoleSubscription.windows.filter(w => w.metric === "counter") };
  panel.monitor.subscriptions = [s];
  const row = byID(panel.render(), `quota-${s.id}`);
  assert.equal(text(byID(row, `quota-${s.id}-usage`)), "$12.50");
});

test("a provider window name keeps its scope visible", () => {
  const { panel, cx, timers } = fixture();
  const s = { ...consoleFixture.consoleSubscription, id: "claude-scoped", provider: "anthropic", name: "Claude",
    primaryWindowId: "seven_day_fable",
    windows: [{ id: "seven_day_fable", metric: "quota", unit: "percent", name: "Неделя", label: "Неделя",
      usedPercent: 10, remainingPercent: 90, used: null, limit: null, remaining: null, counters: {},
      limited: null, observedAt: null, startedAt: null, resetsAt: null, durationSeconds: 604800,
      period: "week", scope: "Fable" }] };
  panel.monitor.subscriptions = [s];
  panel.setOpen(s.id, true, cx); timers[0].callback(cx);
  const detail = panel.render();
  const window = byID(detail, `quota-window-${s.id}-seven_day_fable`);
  assert.ok(text(window).includes("Неделя · Fable"), text(window));
});
