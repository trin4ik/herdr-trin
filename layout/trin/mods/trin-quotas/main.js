import { View, div, svg } from "gpui-kit";
import { h_flex, v_flex, Popover } from "gpui-base";
import { theme, layout, nextCommand, shared, setShared } from "herdr";
import { usage, compactPercent, primaryWindow, quotaColor, hasWarning, balanceText } from "./metrics.js";
import { QuotaMonitor, REFRESH_MS } from "./monitor.js";
import { requestQuotas, quotaURL } from "./transport.js";
import { quotaDetails } from "./details.js";

function find(node, id) {
  if (node?.id === id) return node;
  for (const child of node?.children ?? []) {
    const found = find(child, id);
    if (found) return found;
  }
  return null;
}

export default class Quotas extends View {
  init(props, cx) {
    this.expanded = null;
    this.revealed = null;
    this.disclosureVersion = 0;
    this.closed = false;
    this.defaultEndpoint = props?.endpoint;
    this.makeMonitor = () => new QuotaMonitor(() => requestQuotas(shared("trin.quotas.endpoint") ?? this.defaultEndpoint));
    this.monitor = this.makeMonitor();
    this.monitor.refresh(cx);
    cx.timer.every(REFRESH_MS, cx => this.monitor.refresh(cx));
    // Native commands also make disclosure accessible without pointer input.
    // No polling, new monitor or provider request is started by opening details.
    cx.spawn(async cx => {
      while (!this.closed) {
        const command = await nextCommand();
        if (this.closed) return;
        if (command === "reset-endpoint" || command.startsWith("endpoint--")) {
          try {
            const endpoint = command === "reset-endpoint" ? null : quotaURL(decodeURIComponent(command.slice(10)));
            setShared("trin.quotas.endpoint", endpoint);
            this.monitor.close();
            this.expanded = this.revealed = null;
            this.disclosureVersion++;
            this.monitor = this.makeMonitor();
            this.monitor.refresh(cx);
            cx.notify();
          } catch {
            // Invalid configuration must not replace the last working endpoint.
          }
        } else if (command === "close-details" && this.expanded) this.setOpen(this.expanded, false, cx);
        else if (command.startsWith("details-")) {
          const id = command.slice(8);
          if (this.monitor.subscriptions.some(s => s.id === id)) this.setOpen(id, this.expanded !== id, cx);
        }
      }
    });
  }

  close() { this.closed = true; this.disclosureVersion++; this.monitor?.close(); }

  setOpen(id, open, cx) {
    if (this.closed || (!open && this.expanded !== id)) return;
    const version = ++this.disclosureVersion;
    this.revealed = null;
    if (open) this.expanded = id;
    cx.notify();
    // Establish the collapsed native frame once, then GPUI owns every tween.
    // Version guards make repeated clicks/provider switching interruption-safe.
    cx.timer.after(open ? 16 : 150, cx => {
      if (this.closed || this.disclosureVersion !== version || this.expanded !== id) return;
      if (open) this.revealed = id;
      else this.expanded = null;
      cx.notify();
    });
  }

  disclosure(subscription, t, narrow, viewportHeight) {
    const open = this.expanded === subscription.id;
    return Popover.new(`quota-disclosure-${subscription.id}`)
      .open(open).anchor("bottom_left")
      .on_open_change((open, cx) => this.setOpen(subscription.id, open, cx))
      .trigger(this.row(subscription, t, narrow).cursor_pointer()
        .accessibility_label(`${subscription.name}: подробности лимитов`)
        .hover(el => el.bg(t.hover)))
      .when(open, el => el.content(quotaDetails(subscription, this.monitor, t,
        this.providerIcon(subscription, t, 22), this.revealed === subscription.id,
        cx => this.setOpen(subscription.id, false, cx), viewportHeight)));
  }

  mark(subscription, t, size = 16) {
    const provider = subscription.provider;
    if (["openai", "anthropic", "ollama", "kimi", "opencode"].includes(provider)) {
      // Existing real asset copies also used by Agents (external mod symlinks
      // cannot be used as SVG assets through the native asset sandbox).
      return h_flex().size(size).flex_none().items_center().justify_center()
        .child(svg(`assets/trin-providers/${provider}.svg`).size(size + 4).text_color(t.muted));
    }
    // Unknown providers retain an identifiable fallback.
    return h_flex().size(size).flex_none().items_center().justify_center()
      .text_size(12).font_semibold().text_color(t.muted)
      .child(subscription.name[0]?.toUpperCase() ?? "?");
  }

  meter(t, value) {
    return div().flex_1().min_w(0).h(6).rounded(3).bg(t.hover).overflow_hidden()
      .when(value != null && value > 0, el => el.child(div().h(6).w(`${value}%`).rounded(3).bg(quotaColor(t, value)).opacity(0.75)));
  }

  providerIcon(subscription, t, size = 16) {
    return div().relative().size(size).flex_none().child(this.mark(subscription, t, size))
      .when(Boolean(this.monitor.error || hasWarning(subscription, new Date().toISOString())), el => el.child(div().id(`quota-${subscription.id}-error`)
        .absolute().top(-1).right(-2).size(5).rounded(3).bg(t.ansi3)));
  }

  row(subscription, t, narrow) {
    const value = usage(primaryWindow(subscription));
    // A pay-as-you-go subscription has no percentage: it shows its balance.
    const text = value != null ? compactPercent(value) : balanceText(subscription.balances?.[0]);
    if (narrow) {
      return v_flex().id(`quota-${subscription.id}`).h(34).flex_none().items_center().justify_center().gap(2)
        .child(this.providerIcon(subscription, t, 22))
        .child(div().id(`quota-${subscription.id}-usage`).h(10).line_height("10px").flex_none()
          .text_size(9.5).font_medium().font_family("JetBrains Mono")
          .text_color(value != null && value >= 60 ? quotaColor(t, value) : t.foreground)
          .child(text));
    }

    return h_flex().id(`quota-${subscription.id}`).h(28).flex_none().px(16).min_w(0).items_center()
      .child(this.providerIcon(subscription, t).mr(12))
      .child(this.meter(t, value).mr(12))
      .child(div().id(`quota-${subscription.id}-usage`).flex_none().text_right().font_family("JetBrains Mono").text_size(11)
        .text_color(value != null && value >= 60 ? quotaColor(t, value) : t.foreground).child(text));
  }

  render() {
    const t = theme(), narrow = (find(layout(), "sidebar")?.size?.px ?? 268) < 160;
    const viewportHeight = globalThis.window?.viewport_size?.()?.height;
    if (this.expanded && !this.monitor.subscriptions.some(s => s.id === this.expanded)) {
      this.expanded = null;
      this.revealed = null;
      this.disclosureVersion++;
    }
    return v_flex().size_full().min_h(0).pt(narrow ? 10 : 0).pb(narrow ? 9 : 8).bg(t.chrome).border_t(1).border_color(t.border).text_size(11)
      .when(!narrow, el => el.child(h_flex().h(28).flex_none().pl(16).pr(10).gap(6).items_center().text_color(t.dim)
        .child(div().flex_1().min_w(0).truncate().child("Подписки"))
        .when(!narrow, el => el.child(div().text_size(9).text_color(this.monitor.error ? t.ansi3 : t.dim)
           .child(this.monitor.error ? "ошибка" : "расход")))))
      .child(v_flex().flex_1().min_h(0).overflow_y_scrollbar()
        .when(narrow, el => el.gap(8))
         .when(!this.monitor.subscriptions.length, el => el.child(div().px(narrow ? 2 : 16).py(8).text_size(10).text_color(t.dim)
          .tooltip(this.monitor.error ? "Сервис недоступен" : this.monitor.busy ? "Загрузка…" : "Нет подписок")
          .child(narrow ? "—" : this.monitor.error ? "Сервис недоступен" : this.monitor.busy ? "Загрузка…" : "Нет подписок")))
        .children(this.monitor.subscriptions.map(s => this.disclosure(s, t, narrow, viewportHeight))));
  }
}
