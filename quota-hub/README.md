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
| OpenCode Go | Credential `opencode` из OpenCode `/api/credential` → Go usage opencode.ai | Только платный allowance; сессия не обновляется |

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

## OpenCode Go

Подписка `provider: opencode`, product `opencode_go`, читает только платные
метры Go (5ч/неделя/месяц), `GET https://opencode.ai/zen/go/v1/usage`. Кред — активная
интеграция `opencode` локального сервиса OpenCode: session token или
персональный API-ключ Console. Origin Console зашит в код и никогда не берётся
из credential, поэтому кред не может перенаправить запрос квот.

На машине сбора OpenCode должен быть залогинен в Console: `opencode auth login`
или перенос существующего входа `opencode auth export | opencode auth import`.
Сессия Console не обновляется сборщиком (refresh запрещён): при 401 подписка
сообщает `auth_required` и её нужно переоткрыть входом на этой машине.

Сборщик публикует ровно три конечных окна: `go_rolling`, `go_weekly`,
`go_monthly`; UI показывает 5ч в строке и все окна по клику. Проценты и даты
берутся из ответа, календарный месяц не подменяется 30 днями. Тариф/профиль
по лимитам не угадываются. Отсутствие Go (403), невалидный или частичный ответ —
ошибка, а не нулевой расход; last-good сохраняется с исходной свежестью.
429 учитывает Retry-After/backoff. SSE/event-контракт Go не подтверждён,
поэтому usage polling по умолчанию раз в 5 минут, timeout 15с.

Старый free/refusal bridge, счётчики моделей, баланс и бюджеты удалены из этого
сборщика. При обновлении оставить прежнюю Console-запись выключенной и создать
новую `id: opencode-go`, `provider: opencode`, без `observed_file`/`observe_interval`.
Product изменился: прежний ID с сохранённым state не переиспользовать — store
защищает fingerprint provider/product/source. История Console остаётся на диске,
новые Go-лимиты не наследуют её free-счётчики. Source `opencode_console` сохранён
как имя канала получения. Файлы старых наблюдений
не читаются; bridge продолжает обслуживать только Claude. Авторизация,
история и настройки остальных подписок не удаляются.

## Учёт токенов OpenCode

Две части, общий каталог `~/.local/state/quota-usage/` (или `$XDG_STATE_HOME/quota-usage`):

1. Плагин [tools/opencode-usage-bridge](tools/opencode-usage-bridge/README.md) на каждый
   шаг модели дописывает строку в `events/ГГГГ-ММ-ДД.jsonl`: сессия, модель, токены.
2. `quota-usage` раз в 5 минут дочитывает новые строки, убирает дубли по id события,
   считает API-эквивалент в USD по models.dev и дописывает `ledger/ГГГГ-ММ-ДД.jsonl`.

Цена фиксируется в момент импорта и не пересчитывается; версия снимка models.dev
сохраняется в каждой записи (`prices/`, проверка через ETag). Формула и тиры по
контексту — как у самого OpenCode. `quota-usage -summary` показывает последнюю сводку
(окна 5ч и 24ч), `-report -from … -to …` — сумму за произвольный период.

Не известны: аккаунт конкретного вызова (OpenCode его не отдаёт) и модель вспомогательных
вызовов (title/compaction) — такие записи остаются без цены, а не с нулём.
Плагин пишет без fsync: при падении ОС могут потеряться последние секунды.

```sh
go build -o quota-usage ./cmd/quota-usage
./quota-usage            # сервис: цикл каждые 5 минут
./quota-usage -once      # один цикл, сводка в stdout
```

Локальная выкладка и откат — [RELEASING.md](RELEASING.md).

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

## Deploy в Linux user systemd

`scripts/deploy.sh` предназначен для checkout этой монорепы на машине сервиса;
это не Compose deploy. Требуются Linux, доступный user systemd unit, Git, Go,
curl, flock (util-linux) и стандартные shell utilities. Unit должен запускать
`quota-hub/bin/quota-hub` из этого checkout. Перед запуском убедитесь, что его
конфигурация и `HEALTH_URL` относятся именно к этому unit:

```sh
CONFIG=/absolute/path/to/quota-hub.local.json \
HEALTH_URL=http://127.0.0.1:8098/readyz \
  ./quota-hub/scripts/deploy.sh
```

`CONFIG` обязателен: укажите **тот же файл**, что использует unit. Он проверяется
новым бинарником через `-check-config` до замены; содержимое не выводится и
credentials/state не читаются. Проверка не подтверждает окружение и доступность
источников. `SERVICE` задаёт имя user unit (default `quota-hub`), `GO` — путь к
Go, `SKIP_PULL=1` пропускает pull. Иначе выполняется `git pull --ff-only` с
настроенного upstream; проверку доверия к источнику выполняет оператор.

Скрипт блокирует параллельные запуски из этого checkout, сохраняет предыдущий
бинарник и атомарно заменяет его. При ошибке restart или readiness он пытается
вернуть бинарник, перезапустить и проверить unit. При отсутствии предыдущего
бинарника откат невозможен; при неудачном откате backup остаётся в `bin/.deploy.*`.
Исходники после pull, конфигурация и private state **не откатываются**. При
прерывании процесса вручную может потребоваться восстановление оператором.

`HEALTH_URL` должен оканчиваться на `/readyz`, возвращать HTTP 200 без redirect и
относиться к проверяемому unit; скрипт отдельно проверяет `systemctl is-active`.
`/readyz` означает только, что private store открыт и восстановлен: отсутствие
учётных данных провайдера или ошибка квот сами по себе readiness не блокируют.
Проверка URL не удостоверяет идентичность процесса: не направляйте её на proxy
или другой сервис. Убедитесь в соответствии unit, конфига и endpoint до deploy.

## Offline-проверки

Go 1.27+, Node для bridge tests. Из `quota-hub/`:

```sh
go test -race ./...
go vet ./...
go run ./cmd/quota-hub -config configs/docker.json -check-config
node --test tools/opencode-bridge/test/*.test.mjs
node --test tools/opencode-usage-bridge/test/*.test.mjs
python3 -m unittest discover -s scripts -p 'test_deploy.py'
```

`-check-config` не читает credentials/state и не обращается к сети.
`-once` **делает реальные запросы**, это не validation-команда.
