import { div, svg } from "gpui-kit";
import { h_flex, v_flex } from "gpui-base";
import { usage, remaining, percent, primaryWindow, quotaColor, hasWarning, resetLabel, updatedLabel, planLabel, isCounter, counterRows, windowTitle, unlimited, scopeLabel } from "./metrics.js";

// The height budget uses the same fixed line heights/padding as the builders.
// Scroll owns overflow; individual windows and 4px tracks must never shrink.
const HEADER = 64, FOOTER = 77, MAX_HEIGHT = 600;
const expired = (window, now) => Boolean(window.resetsAt && Date.parse(window.resetsAt) <= Date.parse(now));
const separator = (subscription, index) => index < subscription.windows.length - 1 || Boolean(subscription.balances?.length);
const windowHeight = (window, now, divided) => {
  // A counter window is a short list of absolute values, not a bar.
  if (isCounter(window)) return 46 + counterRows(window).length * 16 + (divided ? 1 : 0);
  return 136 + (expired(window, now) ? 20 : 0) + (divided ? 1 : 0);
};
const warningMessage = (subscription, monitor, now) => monitor.error?.message || subscription.error?.message
  || (hasWarning(subscription, now) ? "Данные могут быть устаревшими" : null);

export function detailsHeight(subscription, monitor = {}, viewportHeight, now = new Date().toISOString()) {
  const balances = subscription.balances?.length ?? 0;
  const content = subscription.windows.length
    ? subscription.windows.reduce((height, window, index) => height + windowHeight(window, now, separator(subscription, index)), 0)
    : 56;
  const balanceHeight = balances ? 24 + balances * 20 + (balances - 1) * 8 : 0;
  const cap = Number.isFinite(viewportHeight) && viewportHeight > 0
    ? Math.max(200, Math.min(MAX_HEIGHT, Math.floor(viewportHeight - 24))) : MAX_HEIGHT;
  return Math.min(cap, HEADER + FOOTER + 2 + content + balanceHeight + (warningMessage(subscription, monitor, now) ? 40 : 0));
}

function dateLabel(value) {
  return value ? updatedLabel(value).replace(", ", " · ") : "Не сообщено";
}

export function balanceLabel(balance) {
  const parts = String(balance.value).split(".");
  if (!/[eE]/.test(parts[0])) parts[0] = parts[0].replace(/\B(?=(\d{3})+(?!\d))/g, "\u202f");
  const value = parts.join(".");
  return `${value}${balance.unit && balance.unit !== "credits" ? ` ${balance.unit}` : ""}`;
}

export function quotaDetails(subscription, monitor, t, icon, revealed, close, viewportHeight) {
  const now = new Date().toISOString(), height = detailsHeight(subscription, monitor, viewportHeight, now);
  const item = (label, value) => h_flex().w_full().min_w(0).h(16).flex_none().gap(10).items_center().text_size(10.5).line_height("16px")
    .child(div().w(65).flex_none().text_color(t.muted).child(label))
    .child(div().flex_1().min_w(0).text_right().text_color(t.foreground).child(value));
  const primary = primaryWindow(subscription), plan = planLabel(subscription.plan);
  const profile = `${subscription.accountEmail || "Аккаунт не указан"}, ${plan || "тариф не указан"}`;
  const warning = warningMessage(subscription, monitor, now);

  return div().id(`quota-details-${subscription.id}`).w(344).h(revealed ? height : 0).flex_none()
    .overflow_hidden().rounded(10).bg(t.popover).border(1).border_color(t.border)
    .opacity(revealed ? 1 : 0)
    .transition("height", { duration: revealed ? 220 : 150, easing: "ease-out" })
    .transition("opacity", { duration: revealed ? 160 : 100, easing: "ease-out" })
    .child(v_flex().w_full().h(height - 2).min_h(0)
      .child(h_flex().id(`quota-profile-${subscription.id}`).h(HEADER).flex_none().px(16).py(12).gap(10).items_start().border_b(1).border_color(t.border)
        .child(icon)
        .child(v_flex().flex_1().min_w(0).gap(4)
          .child(div().h(18).line_height("18px").flex_none().font_semibold().text_size(13).text_color(t.foreground).truncate().child(subscription.name))
          .child(h_flex().id(`quota-account-${subscription.id}`).h(16).line_height("16px").flex_none().min_w(0).overflow_hidden().text_size(10.5).text_color(t.muted)
            .accessibility_label(profile).tooltip(profile)
            .child(div().min_w(0).truncate().child(subscription.accountEmail || "Аккаунт не указан"))
            .child(div().flex_none().child(", "))
            .child(div().id(`quota-tier-${subscription.id}`).flex_none().max_w("50%").truncate().font_medium()
              .text_color(plan ? t.foreground : t.muted).child(plan || "тариф не указан"))))
        .child(div().id(`quota-close-${subscription.id}`).size(24).flex_none().flex().items_center().justify_center()
          .rounded(5).text_color(t.muted).cursor_pointer().hover(el => el.bg(t.hover).text_color(t.foreground))
          .accessibility_label("Закрыть подробности подписки").tooltip("Свернуть · Escape")
          .on_click((_event, cx) => { cx.stop_propagation(); close(cx); })
          .child(svg("mods/workspace-chrome/x.svg").size(13))))
      .child(v_flex().id(`quota-windows-${subscription.id}`).flex_1().min_h(0).overflow_y_scrollbar().px(16)
        .when(Boolean(warning), el => el.child(div().id(`quota-warning-${subscription.id}`).h(40).flex_none().py(12)
          .child(div().h(16).line_height("16px").text_size(10.5).text_color(t.ansi3).truncate()
            .tooltip(`${subscription.windows.length ? "Последние известные данные" : "Данные не подтверждены"}. ${warning}`)
            .child(`${subscription.windows.length ? "Последние известные данные" : "Данные не подтверждены"} · ${warning}`))))
        .when(!subscription.windows.length, el => el.child(div().h(56).flex_none().py(20).text_size(11).line_height("16px").text_color(t.muted)
          .child("Сервис пока не сообщил лимиты этой подписки.")))
        .children(subscription.windows.map((window, index) => {
          const value = usage(window), divided = separator(subscription, index);
          const title = windowTitle(window);
          if (isCounter(window)) {
            const rows = counterRows(window);
            return v_flex().id(`quota-window-${subscription.id}-${window.id}`).h(windowHeight(window, now, divided)).flex_none().py(12).gap(6)
              .when(divided, el => el.border_b(1).border_color(t.border))
              .child(h_flex().h(16).flex_none().gap(8).items_center().line_height("16px")
                .child(div().flex_1().min_w(0).truncate().text_size(11.5).font_semibold().text_color(t.foreground).tooltip(title).child(title))
                .when(scopeLabel(window.scope), el => el.child(div().text_size(9).text_color(t.dim).child(scopeLabel(window.scope)))))
              .child(v_flex().flex_none().gap(4)
                .children(rows.map(([label, text]) => item(label, text))));
          }
          return v_flex().id(`quota-window-${subscription.id}-${window.id}`).h(windowHeight(window, now, divided)).flex_none().py(12).gap(6)
            .when(divided, el => el.border_b(1).border_color(t.border))
            .child(h_flex().h(16).flex_none().gap(8).items_center().line_height("16px")
              .child(div().flex_1().min_w(0).truncate().text_size(11.5).font_semibold().text_color(t.foreground).tooltip(title).child(title))
              .when(window === primary, el => el.child(div().text_size(9).text_color(t.dim).child("В строке"))))
            .child(h_flex().h(16).flex_none().items_center().justify_between().text_size(11).line_height("16px")
              .child(div().text_color(t.muted).child(unlimited(window) ? "Без лимита" : "Израсходовано"))
              .child(div().font_family("JetBrains Mono").font_medium().text_color(value == null ? t.dim : quotaColor(t, value)).child(percent(value))))
            .child(div().id(`quota-track-${subscription.id}-${window.id}`).h(4).flex_none().w_full().rounded(2).bg(t.hover).overflow_hidden()
              .when(value != null && value > 0 && !unlimited(window), el => el.child(div().h(4).flex_none().w(`${value}%`).rounded(2).bg(quotaColor(t, value)).opacity(0.75))))
            .child(h_flex().h(16).flex_none().justify_between().text_size(10.5).line_height("16px")
              .child(div().text_color(t.muted).child(unlimited(window) ? "Ограничение не действует" : `Осталось ${percent(remaining(window))}`))
              .child(div().text_color(t.dim).child(resetLabel(window.resetsAt, now))))
            .child(v_flex().h(36).flex_none().gap(4)
              .child(item("Начало", dateLabel(window.startedAt)))
              .child(item("Сброс", dateLabel(window.resetsAt))))
            .when(expired(window, now), el => el.child(div().h(14).flex_none().line_height("14px")
              .text_size(10).text_color(t.ansi3).child("Срок прошёл · ждём снимок сервиса")));
        }))
        .when(Boolean(subscription.balances?.length), el => el.child(v_flex().id(`quota-balances-${subscription.id}`).flex_none().py(12).gap(8)
          .children(subscription.balances.map((balance, index) => h_flex().id(`quota-balance-${subscription.id}-${index}`).h(20).flex_none().gap(10).items_center().text_size(10.5).line_height("20px")
            .child(div().flex_1().min_w(0).truncate().text_color(t.muted).tooltip(balance.label).child(balance.label))
            .child(div().flex_none().max_w("55%").truncate().text_color(t.foreground).tooltip(`${balance.value} ${balance.unit}`).child(balanceLabel(balance))))))))
      .child(v_flex().id(`quota-freshness-${subscription.id}`).h(FOOTER).flex_none().px(16).py(12).gap(4).border_t(1).border_color(t.border)
        .child(item("Обновлено", dateLabel(subscription.observedAt)))
        .child(item("Получено", dateLabel(monitor.fetchedAt)))
        .child(div().h(12).flex_none().line_height("12px").text_size(9.5).text_color(t.dim).child("Время — МСК · обновление раз в минуту"))));
}
