# quota-hub

Headless Go-сборщик → private last-good/history/cooldown → нормализованный HTTP
снимок. Только stdlib, без генераций, OAuth refresh и записи в credential stores.
Контракт: [../docs/quota-api.md](../docs/quota-api.md).

| Провайдер | Реальный источник | Ограничение |
| --- | --- | --- |
| Codex | Native `.codex/auth.json`, access token/account → usage GET | Не любой OpenCode token-sharing вход; expiry обновляет native CLI |
| Claude | Passive: `rate_limit_event` ответов провайдера `claude-agent` (Claude Code в OpenCode) через `opencode-usage-bridge` или headers провайдера `anthropic`; опционально native usage GET | Без `claude_fallback` запросов к Anthropic нет; данные обновляются только ответами Claude |
| Claude proxy | Отдельный read-only HTTPS `/v1/subscription-limits` с выделенным Bearer | Нет данных/ошибка = unavailable или stale; секрет только в env |
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

Для каждого Claude-контейнера добавьте отдельную подписку как в
`configs/example.json`: `provider: claude`, `source: claude_proxy`,
уникальные `id` и безопасный `integration_id` (не `anthropic`), `remote_url` ровно HTTPS
`/v1/subscription-limits`, `token_env` — имя env-переменной,
`display_name` (опционально, до 60 символов без control characters).
Необязательный `verified_plan` (например, `pro`) задавайте только по
подтверждённому `claude auth status` этого контейнера; изменяйте при смене
аккаунта/тарифа. Это метка, не вывод из процентов и не email.
`verified_account_email` — отдельный необязательный адрес из штатного
`claude auth status` **того же контейнера**. Хранить только в приватном
runtime config. При смене аккаунта сначала убрать старый адрес и квоту;
адрес появляется в API лишь с `publish_account_email: true`.
Имя локальное, не является идентичностью и публикуется как optional
`display_name` в `/v1/quotas`. В JSON хранится только имя; задайте сам Bearer
в окружении сервиса. Источник не читает SSH,
Claude credentials или старый bridge. Ответ `no_data` и любые сетевые/HTTP
ошибки не становятся нулевой квотой. Usage связывается через
`opencode_provider`, значение которого равно `integration_id` и не равно
`"anthropic"`.

Claude account metadata/email требует отдельного account_file (`.claude.json`)
и ожидаемого SHA256 account UUID в claude_fallback; default не монтирует этот
файл с project history и не включает account pin. Не путайте unknown profile с
ошибкой квоты. Подключение профиля — явное локальное решение, не auto-discovery.

Пассивная подписка без запросов к Anthropic (только ответы Claude Code через
`opencode-usage-bridge`): без `claude_fallback`, с `"opencode_provider": "claude-agent"`,
чтобы вызовы этого провайдера считались в окнах, и с блоком `claude_profile`
(`credential_file`, `account_file`, `expected_account_sha256`). Профиль (e-mail и
`subscriptionType`) читается только из локальных файлов Claude Code; истёкший
access token скрывает профиль до обновления самим Claude Code, квоты остаются.
Смена `opencode_provider` перезаписывает текущие окна в `windows.jsonl`, вызовы
нового провайдера считаются с этого момента. Модели-псевдонимы `claude-agent`
(`opus`, `sonnet`, `haiku`, `fable`) оцениваются по самой новой модели семейства
у `anthropic` в models.dev; это оценка, а не точный ID модели вызова.

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
берутся из ответа, календарный месяц не подменяется 30 днями. Тариф `Go`
показывается только со свежим подтверждённым ответом платного endpoint, не
вычисляется из размеров лимитов. Отсутствие Go (403), невалидный или частичный
ответ — ошибка, а не нулевой расход; last-good сохраняется с исходной свежестью.
429 учитывает Retry-After/backoff. SSE/event-контракт Go не подтверждён,
поэтому usage polling по умолчанию раз в 5 минут, timeout 15с.

Email по умолчанию скрыт. При `publish_account_email=true` сборщик может
показать email, если штатный OAuth-вход в официальный Console сохранил его с
ID аккаунта [в метаданных активного credential](https://github.com/anomalyco/opencode/blob/4d4a95a3239c14ceb4aaa8d6b955b0cdd6f1908c/packages/core/src/plugin/provider/opencode.ts#L825-L854).
После получения квоты сборщик повторно сверяет локальный credential; без
подтверждения или после рестарта до следующей проверки адрес скрыт. Для API
ключей и старых записей без этих метаданных адрес не угадывается. Чтобы
опубликовать его, нужен штатный OAuth-вход, сохранивший метаданные, и явный
согласованный opt-in; новых запросов профиля к Console сборщик не делает.

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
контексту — как у самого OpenCode, включая режимы моделей из models.dev
(`gpt-6-luna-fast` = режим `fast` модели `gpt-6-luna` со своей ценой). `quota-usage -summary` показывает последнюю сводку
(окна 5ч и 24ч), `-report -from … -to …` — сумму за произвольный период.

Только если провайдера события нет в каталоге, импортёр ищет модель по точному
ID и берёт цену у исходного провайдера, указанного в `canonical_model_id`.
Если ссылки расходятся, оригинальный тариф отсутствует или ссылка ведёт на
другую модель (например, базовую вместо Fast), вызов остаётся без оценки.
Цена посредника не подставляется. Провайдер события для учёта квот не меняется.

Опциональный `-quotas-url http://localhost:8098/v1/quotas` после импорта читает
нормализованный снимок без авторизации (таймаут 10с, до 1 MiB). Связь задаётся
`opencode_provider` подписки, без справочника моделей в импортёре.
`windows.jsonl` хранит экземпляры окон, `state.json` — последние границы;
дрожание reset_at до ±120с не создаёт дубль. `summary.current` показывает суммы
ledger по этим окнам, `unmatched` — вызовы неизвестных подпискам провайдеров за 24ч;
ошибка снимка остаётся в `quotas.error`, сохраняя последний успешный ответ.
`quota-usage -windows` выводит все записанные экземпляры с суммами, без импорта.

Чтобы показывать API-эквивалент в подробностях квот, явно задайте в конфиге
quota-hub верхнеуровневый `usage_summary_file`: абсолютный путь к `summary.json`
этого импортёра. Пустая строка или отсутствие поля выключает функцию. Файл
читается при HTTP-снимке, кэшируется по mtime/size, предел — 4 MiB. Ошибка файла
не ломает квоты: оценка отсутствует. Сводка старше 15 минут показывается
приглушённой; это локальная оценка по ценам API, а не счёт провайдера.
При неизвестном начале считаем от первого наблюдения (`partial:true`), месяц
не заменяем 30 днями. Без `-quotas-url` прежние окна 5ч/24ч работают без изменений.

Учитываются только шаги сессий. Заголовки, сжатие OpenCode и вызовы плагинов через
`generate` не учитываются: OpenCode не отдаёт плагинам их расход. Аккаунт вызова
OpenCode тоже не отдаёт: привязка к подписке идёт по провайдеру, не по аккаунту.
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
Claude proxy использует отдельный polling worker и независимый state по ID;
его прокси `observed_at` — время наблюдения, оно не подменяется временем GET.
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
