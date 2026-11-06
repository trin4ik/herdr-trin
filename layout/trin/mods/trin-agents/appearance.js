// Native GPUI presentation. State, timers, commands and grouping live in main.js.
import { div, svg } from "gpui-kit";
import { h_flex } from "gpui-base";
import { shared } from "herdr";
import { formatTokens, formatActiveTime, modelLabel } from "./metrics.js";
import { contextTooltip, subagentsTooltip, shellsTooltip } from "./tooltips.js";

// Matches Spaces; kept mod-local so Agents can be distributed independently.
const MARK = { ansi6: "#22a99a", ansi3: "#e3814f", ansi4: "#5b8def", ansi5: "#8b7cf6", ansi2: "#3fae6a", ansi1: "#d9649a" };
const LINE_HEIGHT = 18;
const ICON_GAP = 7;

export function spaceAvatar(space, t) {
  const color = space?.color ?? (shared("space.colors") ?? {})[space?.id] ?? "ansi6";
  const fill = color.startsWith("#") ? color : (MARK[color] ?? t[color]);
  return h_flex().size(LINE_HEIGHT).flex_none().rounded(5).bg(fill).items_center().justify_center()
    .text_size(10.5).font_bold().text_color("#ffffff")
    .tooltip(space?.name || "Space")
    .child([...(space?.name ?? "")][0]?.toUpperCase() ?? "?");
}

// compact: a ! for blocked where a header already says "Needs you".
export function statusIndicator(t, status, compact = false) {
  const color = t[status] ?? t.dim;
  if (status === "working") return svg("mods/workspace-chrome/spinner.svg").size(12).mx(1).flex_none().text_color(t.working).spin(900);
  if (status === "blocked" && compact) return div().size(14).flex_none().rounded(4).bg(t.blocked + "26").text_size(10.5).font_bold().text_color(t.blocked).flex().items_center().justify_center().child("!");
  if (status === "blocked") return div().flex_none().px(6).h(LINE_HEIGHT).rounded(5).bg(t.blocked + "26").text_size(10.5).font_semibold().text_color(t.blocked).flex().items_center().child("Needs you");
  if (status === "idle") return div().size(7).mx(3).flex_none().rounded(4).border(1.5).border_color(t.idle ?? t.dim);
  return div().size(7).mx(3).flex_none().rounded(4).bg(status === "done" ? color : t.dim + "00");
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

export function sessionDetails(t, meta, activity, elapsed, address) {
  const provider = meta?.providerID;
  const known = provider === "openai" || provider === "anthropic";
  const model = meta?.modelID;
  const modelTitle = model ? `${provider ?? "?"}/${model}${meta.variant ? ` · ${meta.variant}` : ""}` : "Model data unavailable";
  const timingTitle = "Accumulated session work time; runs continuously while status is Working"
    + (!meta?.durationReady ? "\nHistorical assistant-step total is still loading; showing locally observed work" : "");
  return h_flex().h(LINE_HEIGHT).min_w(0).gap(6).items_center().text_size(12).text_color(t.muted)
    .child(h_flex().id(`agent-${address}-model`).flex_1().min_w(0).gap(ICON_GAP).items_center().tooltip(modelTitle)
      .child(h_flex().w(LINE_HEIGHT).h(LINE_HEIGHT).flex_none().items_center().justify_center()
        // Visible mark ~15px inside the 18px alignment column.
        .when(known, el => el.child(svg(`assets/trin-providers/${provider}.svg`).size(20).flex_none().text_color(t.muted)))
        .when(!known && Boolean(provider), el => el.child(div().font_semibold().child(provider[0].toUpperCase()))))
      .child(h_flex().flex_1().min_w(0).overflow_hidden().gap(3).items_center()
        // Preserve the model's natural width first, capped by the available
        // column so it can still ellipsize after effort has yielded its space.
        .child(div().flex_none().max_w("100%").truncate().child(modelLabel(model)))
        .when(Boolean(meta?.variant), el => el.child(div().min_w(0).flex_shrink(1).overflow_hidden().whitespace_nowrap().text_color(t.ansi3).child(`@${meta.variant}`)))))
    .when(Boolean(activity), el => el.child(activityBadges(t, activity, address)))
    .child(div().id(`agent-${address}-context`).flex_none().text_color(t.ansi4).tooltip(contextTooltip(meta))
      .child(formatTokens(meta?.contextTokens)))
    .child(div().id(`agent-${address}-time`).flex_none().font_family("JetBrains Mono").text_color(t.muted).tooltip(timingTitle)
      .child(elapsed == null ? "…" : formatActiveTime(elapsed)));
}
