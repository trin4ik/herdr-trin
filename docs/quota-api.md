# Контракт Herdr ↔ quota-hub, schema v1

`GET /v1/quotas`, HTTP 200, UTF-8 JSON ≤1MiB. Это готовый снимок, а не команда
upstream refresh. Polling Herdr: сразу и каждые 60с, connect 5с / total 15с.
Default URL `http://127.0.0.1:8098/v1/quotas`; свой адрес в layout block
`quotas.content.props.endpoint` либо `layout/trin/mods/trin-quotas/transport.js`.
Remote только HTTPS без URL credentials/query. Транспорт не отправляет Bearer.
Личный runtime override задаётся командой `trin-quotas.endpoint--<encoded URL>`,
через native shared API, не ручным редактированием shared.json; он выше props.

Минимальный синтетический пример (не реальные аккаунты/квоты):

```json
{
  "schema_version": 1,
  "generated_at": "2026-11-06T12:00:00Z",
  "subscriptions": [{
    "id": "codex-primary",
    "provider": "codex",
    "account_email": null,
    "plan": "pro",
    "status": "ok",
    "source": "native_codex_oauth",
    "last_success_at": "2026-11-06T11:59:00Z",
    "last_attempt_at": "2026-11-06T11:59:00Z",
    "next_attempt_at": "2026-11-06T12:04:00Z",
    "stale_after": "2026-11-06T12:29:00Z",
    "stale": false,
    "error": null,
    "quotas": [{
      "id": "weekly", "used_percent": 25, "remaining_percent": 75,
      "window": { "period": "week", "duration_seconds": 604800,
        "started_at": null, "reset_at": "2026-11-10T12:00:00Z" }
    }],
    "metrics": []
  }]
}
```

## Поля, которые использует UI

- Root: строго `schema_version: 1`, валидная дата `generated_at`, массив subscriptions.
  Пустой массив означает удалить все строки, не продолжать показывать старые.
- Subscription: непустой уникальный string id, string provider, quotas array/null.
  Provider aliases: `codex`/`openai`, `claude`/`anthropic`, `ollama`, `kimi`.
  Неизвестные provider отображаются с fallback-именем/иконкой.
- `plan`, `account_email`, `source` optional/null. Email только явное поле, без
  control characters, trim и ≤300 символов в UI; не выводится из ID/source.
  Backend по умолчанию email не публикует. Тариф не вычисляется из лимитов.
- Quota: непустой id, уникальный внутри подписки; used_percent/remaining_percent
  — finite numbers 0..100 либо null. Если used отсутствует, он может вычисляться
  как 100-remaining. Если оба неизвестны, шкала неизвестна, а не 0%.
- `window.period`: week/day/month или unknown/null; duration_seconds помогает
  определить 5ч/неделю. started_at/reset_at — ISO dates либо null. Календарный
  месяц не подменяется 30 днями. `scope: code|total` создаёт отдельные подписи.
- Несколько окон/пулов не объединяются. Codex primary — weekly, затем 5ч/session/
  первое окно; остальные — 5ч/session/первое. Проценты в строке округлены,
  в деталях сохраняется точность; абсолютные даты показываются UTC+3.
- `metrics[]` с `metric: balance`, id, numeric value, unit — баланс отдельно
  от процента квоты (credits_balance/purchased_balance получают свои подписи).
- last_success_at/last_attempt_at/next_attempt_at/stale_after — timestamps либо
  null; stale boolean, status string. Provider error object: безопасные code,
  message, http_status. Не возвращать raw response, токены, paths или stack traces.

Полный четырёхпровайдерный synthetic fixture:
`layout/trin/tests/fixtures/quota-v1.json`; исполняемый UI-validator —
`layout/trin/mods/trin-quotas/data.js`; backend model —
`quota-hub/internal/model/model.go`. Optional дополнительные backend поля UI
игнорирует. Ошибка HTTP/JSON/schema сохраняет last-good и показывает warning.
