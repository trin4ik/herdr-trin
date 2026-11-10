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
  Provider aliases: `codex`/`openai`, `claude`/`anthropic`, `ollama`, `kimi`, `opencode`.
  Неизвестные provider отображаются с fallback-именем/иконкой.
- `plan`, `account_email`, `source` optional/null. Email только явное поле, без
  control characters, trim и ≤300 символов в UI; не выводится из ID/source.
  Backend по умолчанию email не публикует. Тариф не вычисляется из лимитов.
- Quota: непустой id, уникальный внутри подписки; used_percent/remaining_percent
  — finite numbers 0..100 либо null. Если used отсутствует, он может вычисляться
  как 100-remaining. Если оба неизвестны, шкала неизвестна, а не 0%.
- `metric`: `quota` (по умолчанию, доля лимита) или `counter` (абсолютные
  значения). Для `counter` заполняются `used`/`limit`/`remaining` и `unit`
   (`requests`, `tokens`, `USD`), проценты остаются null. Для события отказа
   допускается `unit: status` без числового used: это не измеренный расход.
   `counters` — optional
  объект с известными числовыми ключами (`input_tokens`, `output_tokens`,
  `cache_read_tokens`, `cost_usd`, `limit_usd`, `spent_usd`); неизвестные ключи
  игнорируются.
- `label` — optional имя окна от провайдера (модель, бюджет, вид лимита). UI
  показывает его вместо выведенного подписи окна. `scope` рядом с ним задаёт
  пул (модель, «free», «code»).
- `limited: false` — провайдер сообщает, что окно не ограничено: UI не рисует
  его как полностью израсходованное. Это не синоним «сейчас запросы разрешены»:
  Go allowance остаётся конечным и при status=ok.
- `observed_at` — optional время последнего подтверждения этого окна. Окна одной
  подписки могут приходить из источников с разной частотой (пассивные заголовки
  и нативный usage-endpoint), поэтому окно может быть старше самой подписки.
- `window.period`: week/day/month или unknown/null; duration_seconds помогает
  определить 5ч/неделю. started_at/reset_at — ISO dates либо null. Календарный
  месяц не подменяется 30 днями. `scope: code|total` создаёт отдельные подписи.
- Несколько окон/пулов не объединяются. Codex primary — weekly, затем 5ч/session/
  первое окно; остальные — 5ч/session/первое. Для `opencode` primary —
  Go weekly/monthly/rolling, затем первое окно; отказ остаётся событием без
  процента, строка без измеренного allowance показывает баланс. Проценты в строке округлены,
  в деталях сохраняется точность; абсолютные даты показываются UTC+3.
- `metrics[]` с `metric: balance`, id, numeric value, unit — баланс отдельно
  от процента квоты (credits_balance/purchased_balance/credit_limit получают
  свои подписи).
- last_success_at/last_attempt_at/next_attempt_at/stale_after — timestamps либо
  null; stale boolean, status string. Provider error object: безопасные code,
  message, http_status. Не возвращать raw response, токены, paths или stack traces.

## Источники и окна

- Claude Code: нативный usage-endpoint возвращает расширяемый массив `limits[]`
  (`kind`, `group`, `percent`, `scope`, `resets_at`). Backend читает его целиком:
  `session` → `five_hour`, `weekly_all` → `seven_day`, `weekly_scoped` →
  `seven_day_<модель>` со scope модели. Неизвестный `kind` сохраняется как
  отдельное окно, а не теряется. Плоские ключи `five_hour`/`seven_day` — только
  fallback для ответа без `limits[]`, поэтому окна не считаются дважды.
- Passive-заголовки Anthropic знают только 5ч и неделю. Окна обоих источников
  объединяются по id: окно, о котором знает только один источник, не исчезает,
  а для общего окна побеждает более свежее значение.
- OpenCode Console: `GET /console/api/billing/status` (баланс),
  `/console/api/usage/models` (счётчики по моделям), `/console/api/policies/current-budgets`
  (бюджеты), `GET /zen/go/v1/usage` (Go-метры; 403 — нет подписки Go, это не ошибка).
  Остаток бесплатных моделей Console не публикует. Passive-половина наблюдает
  HTTP 429 и `Retry-After` (файл `provider_id: opencode`, `rate_limit_reset`).
  Одних этих метаданных недостаточно, чтобы отличить дневной free-cap от Go,
  paid или общего throttling: UI пишет «Отказ провайдера», а не объявляет
  лимит всех бесплатных моделей. Legacy `free_limit_reset` читается с той же
  консервативной семантикой. Проценты/остаток из отказа не вычисляются;
  после времени повтора событие не считается действующим ограничением,
  но восстановление allowance тоже не выдумывается.

Полный четырёхпровайдерный synthetic fixture:
`layout/trin/tests/fixtures/quota-v1.json`; исполняемый UI-validator —
`layout/trin/mods/trin-quotas/data.js`; backend model —
`quota-hub/internal/model/model.go`. Optional дополнительные backend поля UI
игнорирует. Ошибка HTTP/JSON/schema сохраняет last-good и показывает warning.
