# quota-hub

Headless Go-сборщик → private last-good/history/cooldown → нормализованный HTTP
снимок. Только stdlib, без генераций, OAuth refresh и записи в credential stores.
Контракт: [../docs/quota-api.md](../docs/quota-api.md).

| Провайдер | Реальный источник | Ограничение |
| --- | --- | --- |
| Codex | Native `.codex/auth.json`, access token/account → usage GET | Не любой OpenCode token-sharing вход; expiry обновляет native CLI |
| Claude | Passive headers + native usage GET; native `.claude/.credentials.json` | Keychain не читается; нативный GET даёт все окна, включая лимит по модели |
| Ollama | Active key `ollama-cloud` из OpenCode `/api/credential` | Нужен доступ к owning service |
| Kimi | Active key явно выбранной `kimi-code-plan-cn` или `kimi-code-plan-global` | Не угадывать регион по имени аккаунта |
| OpenCode Console | Credential `opencode` из OpenCode `/api/credential` → Console API opencode.ai | Сессия Console не обновляется здесь; нужен повторный вход на машине |

Не PIN/device-code login: сервис использует уже существующий вход и не выполняет
auth switch/refresh. Нет отдельной поддержки произвольного API-key env для
Ollama/Kimi: env `opencode.token_env` относится к Bearer **сервиса OpenCode**,
не ключу провайдера. Секреты не вставляются в JSON config.

## Compose

Из корня: `docker compose up -d --build`. См. основной README и `.env.example`.
Default config `configs/docker.json`: Codex+Claude включены, Ollama/Kimi выключены,
email выключен. API без auth доступен только host loopback и Compose-сети.
Файлы credentials read-only, private state в отдельном volume. Отсутствие файла
или expired token даёт unavailable/stale, а не успешную квоту 0%.

Docker context должен быть **локальным компьютером с credential-файлами**.
Remote Docker daemon не получает файлы bind mounts с вашего Mac: source paths
разрешаются на daemon host. Не запускайте этот Compose в чужом remote context.
Не монтируйте home целиком. На Linux rootless Docker возможны ограничения UID/
доступа к 0600; не исправляйте их chmod 644 на credentials.

Для изменения подписок скопируйте config в `quota-hub/configs/runtime.local.json`
и задайте `QUOTA_CONFIG=./quota-hub/configs/runtime.local.json` в `.env`.
Смена UID/путей/данных должна оставаться локальной; Dockerfile не COPY credentials.
Контейнер запускается root для чтения 0600 host-files; caps dropped, no-new-privileges,
rootfs RO. Это компромисс, не заявление о полной изоляции от контейнерного root.

## Дополнительные источники

Самый простой надёжный способ для OpenCode — запустить Go-бинарник на **той же
машине/namespace**, где работает OpenCode: `configs/example.json` копируется в
`runtime.local.json`, абсолютные пути заменяются своими, подписки включаются явно.
Пример использует Bearer по умолчанию; задайте непустой `QUOTA_HUB_HTTP_TOKEN`
(≥24 bytes) в окружении процесса, не в tracked config. Herdr-транспорт Bearer
пока не отправляет; его local Compose режим использует явный `http_auth=none`.

`opencode.service_file` содержит URL и Basic password локального сервиса.
Не публикуйте его. Reader перечитывает регистрацию, не запускает OpenCode,
не обращается к credential resolver и не копирует его БД.
В Docker URL `127.0.0.1` из host service.json указывает на сам контейнер:
простого монтирования service.json **недостаточно**. Не открывайте credential API
в LAN ради удобства; изолированная сеть и auth требуют отдельной настройки.
Поддержка host networking зависит от ОС/Docker, автоматически не включается.

Опциональный bridge: [tools/opencode-bridge](tools/opencode-bridge/README.md).
Создайте directory bridge у владельца с правами 0700, настройте outputFile,
затем добавьте к сервису в игнорируемом `compose.override.yaml`:

```yaml
services:
  quota-hub:
    volumes:
      - type: bind
        source: ${QUOTA_BRIDGE_DIR:-${HOME}/.local/state/quota-hub/bridge}
        target: /bridge
        read_only: true
        bind:
          create_host_path: false
```

Claude account metadata/email требует отдельного account_file (`.claude.json`)
и ожидаемого SHA256 account UUID в claude_fallback; default не монтирует этот
файл с project history и не включает account pin. Не путайте unknown profile с
ошибкой квоты. Подключение профиля — явное локальное решение, не auto-discovery.

## OpenCode Console (pay-as-you-go)

Подписка `provider: opencode` читает рабочее пространство Console: баланс,
счётчики по моделям, бюджеты и метры Go (5ч/неделя/месяц). Кред — активная
интеграция `opencode` локального сервиса OpenCode: session token или
персональный API-ключ Console. Origin Console зашит в код и никогда не берётся
из credential, поэтому кред не может перенаправить запрос квот.

На машине сбора OpenCode должен быть залогинен в Console: `opencode auth login`
или перенос существующего входа `opencode auth export | opencode auth import`.
Сессия Console не обновляется сборщиком (refresh запрещён): при 401 подписка
сообщает `auth_required` и её нужно переоткрыть входом на этой машине.

Публичного остатка бесплатных моделей Console не отдаёт ни один endpoint.
Единственный факт об исчерпании дневного лимита — отказ 429 с `Retry-After`;
его наблюдает bridge-плагин (второй файл, `provider_id: opencode`, запись
`free_limit_reset`) и backend показывает окно «Бесплатные модели» со сбросом.

## Поведение и доступ

Provider polling по умолчанию 5м с jitter. Claude читает passive file каждые 30с и
опрашивает native OAuth usage тем же интервалом подписки: только native-ответ
знает все окна аккаунта, включая недельный лимит по модели, которого нет в
заголовках. Окна обоих источников объединяются по id. Если событий нет, это
документированное исключение: provider APIs не имеют подтверждённой подписки.
Retry-After и persisted backoff имеют приоритет; 401/403 не запускают refresh.
GET `/v1/quotas` не опрашивает upstream. Ошибка сохраняет last-good с stale.

Проверки здоровья: `/healthz`, `/readyz` (точные HTTP semantics — `internal/httpapi`).
Private storage содержит raw last-good/метаданные и историю; это **не публичный
артефакт**. Автоматическая retention пока отсутствует. Не коммитьте volume/export.
`publish_account_email=true` раскрывает email всем читателям API: включать только
сознательно. Даже без email usage/plan — приватные данные; без публичного port bind.

Wildcard listener по умолчанию запрещён. `allow_wildcard_listen=true` нужен только
для контейнерного listener; это не firewall. Compose loopback bind обязателен.
Для сети — отдельная auth/TLS/ACL, не перенос чужого deployment config.

## Offline-проверки

Go 1.27+, Node для bridge tests. Из `quota-hub/`:

```sh
go test -race ./...
go vet ./...
go run ./cmd/quota-hub -config configs/docker.json -check-config
node --test tools/opencode-bridge/test/*.test.mjs
```

`-check-config` не читает credentials/state и не обращается к сети.
`-once` **делает реальные запросы**, это не validation-команда.
