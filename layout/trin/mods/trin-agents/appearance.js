// Native GPUI presentation. State, timers and commands live in main.js.
import { div, image, svg } from "gpui-kit";
import { h_flex } from "gpui-base";
import { shared } from "herdr";
import { formatTokens, formatActiveTime, modelLabel } from "./metrics.js";
import { contextTooltip, subagentsTooltip, shellsTooltip } from "./tooltips.js";

// Matches Spaces; kept mod-local so Agents can be distributed independently.
const MARK = { ansi6: "#22a99a", ansi3: "#e3814f", ansi4: "#5b8def", ansi5: "#8b7cf6", ansi2: "#3fae6a", ansi1: "#d9649a" };
const LINE_HEIGHT = 18;
const MODEL_GAP = 7;

export function spaceAvatar(space, t) {
  const key = JSON.stringify([space?.machine ?? null, space?.folder ?? ""]);
  const icon = shared("trin.project-icons.ready")?.[key];
  const custom = typeof icon === "string" && /^data\/trin-project-icons\/[a-f0-9]{64}\.png$/.test(icon);
  const color = space?.color ?? (shared("space.colors") ?? {})[space?.id] ?? "ansi6";
  const fill = color.startsWith("#") ? color : (MARK[color] ?? t[color]);
  return h_flex().size(LINE_HEIGHT).flex_none().rounded(5).overflow_hidden().items_center().justify_center()
    .tooltip(space?.name || "Space")
    .when(custom, el => el.child(image(icon).size_full()))
    .when(!custom, el => el.bg(fill).text_size(10.5).font_bold().text_color("#ffffff")
      .child([...(space?.name ?? "")][0]?.toUpperCase() ?? "?"));
}

export function statusIndicator(t, status) {
  const color = t[status] ?? t.dim;
  if (status === "working") return svg("mods/workspace-chrome/spinner.svg").size(12).mx(1).flex_none().text_color(t.working).spin(900).tooltip("В работе");
  if (status === "blocked") return div().size(14).flex_none().rounded(4).bg(t.blocked + "26").text_size(10.5).font_bold().text_color(t.blocked).flex().items_center().justify_center().tooltip("Нужно ваше действие").child("!");
  if (status === "idle") return div().size(7).mx(3).flex_none().rounded(4).border(1.5).border_color(t.idle ?? t.dim).tooltip("Ожидает");
  return div().size(7).mx(3).flex_none().rounded(4).bg(status === "done" ? color : t.dim + "00")
    .when(status === "done", el => el.tooltip("Готово"));
}

function activityBadges(t, info, address) {
  const badge = (icon, count, description) => h_flex().id(`agent-${address}-${icon}`).flex_none().h(LINE_HEIGHT).px(5).gap(4).items_center()
    .rounded(5).bg(t.raised).text_size(11.5).font_medium().text_color(t.muted)
    .opacity(info.stale ? 0.65 : 1)
    .tooltip(description + (info.stale ? "\nLast known counts; refresh is paused or unavailable" : "")
      + (info.pendingZero?.length ? "\nConfirming completion on the next refresh" : ""))
    .child(svg(`mods/workspace-chrome/${icon === "subagents" ? "git-branch" : "terminal"}.svg`).size(12).flex_none().text_color(t.muted))
    .child(String(count));
  return h_flex().flex_none().gap(4).items_center()
    .when(info.subagents > 0, el => el.child(badge("subagents", info.subagents, subagentsTooltip(info))))
    .when(info.shells > 0, el => el.child(badge("shells", info.shells, shellsTooltip(info))));
}

export function modelDescription(meta) {
  if (!meta?.modelID) return null;
  return `${meta.providerID ?? "?"}/${meta.modelID}${meta.variant ? ` · ${meta.variant}` : ""}`
    + (meta.modelFromHistory ? "\nМодель последнего шага OpenCode; модель сессии не задана" : "")
    + (meta.modelStale ? "\nПоследняя известная модель; обновление недоступно" : "");
}

export function sessionDetails(t, meta, activity, elapsed, address, background = false) {
  const model = meta?.modelID;
  const modelTitle = modelDescription(meta) ?? "Model data unavailable";
  const timingTitle = "Accumulated session work time; runs continuously while status is Working"
    + (!meta?.durationReady ? "\nHistorical assistant-step total is still loading; showing locally observed work" : "");
  return h_flex().h(LINE_HEIGHT).min_w(0).gap(6).items_center().text_size(12).text_color(t.muted)
    .child(h_flex().id(`agent-${address}-model`).flex_1().min_w(0).gap(MODEL_GAP).items_center().tooltip(modelTitle)
      // Keep the old mark's column so the model stays aligned with the title.
      .child(div().w(LINE_HEIGHT).h(LINE_HEIGHT).flex_none())
      .child(h_flex().flex_1().min_w(0).overflow_hidden().gap(3).items_center()
        // Preserve the model's natural width first, capped by the available
        // column so it can still ellipsize after effort has yielded its space.
        .child(div().flex_none().max_w("100%").truncate().child(modelLabel(model)))
        .when(Boolean(meta?.variant), el => el.child(div().min_w(0).flex_shrink(1).overflow_hidden().whitespace_nowrap().text_color(t.ansi3).child(`@${meta.variant}`)))))
    .when(Boolean(activity), el => el.child(activityBadges(t, activity, address)))
    .when(background, el => el.child(div().flex_none().child("В фоне")))
    .when(!background, el => el
      .child(div().id(`agent-${address}-context`).flex_none().text_color(t.ansi4).tooltip(contextTooltip(meta))
        .child(formatTokens(meta?.contextTokens)))
      .child(div().id(`agent-${address}-time`).flex_none().font_family("JetBrains Mono").text_color(t.muted).tooltip(timingTitle)
        .child(elapsed == null ? "…" : formatActiveTime(elapsed))));
}
