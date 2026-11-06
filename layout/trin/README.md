# Trin — native layout Herdr

Постоянные Projects, Agents со статистикой, плоские вкладки, Files/Changes справа
и раскрываемые карточки подписок. Это нативные GPUI-моды, не HTML-макеты.

## Состав

- `layouts/trin.json` — окно, sidebar, вкладки, workspace и status/controller.
- `mods/trin-projects` — постоянный реестр; закрытие последней сессии не удаляет
  проект. Папка выбирается явно, новая вкладка запускается из корня проекта.
- `mods/trin-tabs`, `trin-tab-menu` — вкладки, оконные кнопки и меню.
- `mods/trin-agents` — агенты, группировка, модель/effort/context/tasks/time.
- `mods/trin-brand` — шапка Deliva. Фирменный знак не объявлен MIT-ассетом:
  см. `BRAND-ASSET.md`. Право распространения с layout подтверждено владельцем.
- `mods/trin-quotas` — готовый JSON сервиса, без provider credentials.
- `keys.json` — только `layout:trin`, объединять с личными keys.

Старый `trin-spaces` сохранён для rollback/regression tests, но в layout не включён.
Пустые каталоги модов, исследования, скриншоты, generated types и история макетов
в публичный набор не входят. Шесть активных модов включены; `trin-tab-menu`
вызывается другими модами, а не отдельным блоком окна.

## Установка

Нужен установленный Herdr с GPUI API поколения клона `0.1.11` (проверяйте точные
типы своей версии), curl, Python 3.9+ для remote directories/статистики Agents.
Установщик запускается из корня монорепы:

```sh
python3 scripts/install-layout.py --config-dir "$HOME/.config/herdr-app"
herdr-app do layout.switch '{"name":"trin"}'
```

Существующие имена не перезаписываются. Установщик копирует моды/layout, объединяет
только новую секцию keys и устанавливает реальные assets (`trin-brand`,
`trin-projects`, `trin-providers`). Встроенные моды Herdr не копируются и не меняются.
Это fresh-install инструмент, не updater. Сначала проверяйте в отдельной config
директории. Откат окна: `ctrl-alt-shift-escape`.

В разработке можно связать моды/layout с этим checkout symlink-ами; изображения
должны оставаться реальными копиями в assets из-за sandbox. Watcher может не увидеть
изменения внешней цели ссылки: уведомляйте ссылку `touch -h`, не заменяйте её.

## Подписки и настройка адреса

Default: `http://127.0.0.1:8098/v1/quotas` от корневого Compose. Запрос выполняется
на компьютере интерфейса, не на машине выбранного проекта. Для своего сервиса:

```json
{
  "id": "quotas", "type": "block",
  "content": { "mod": "trin-quotas", "props": { "endpoint": "https://quota.example.com/v1/quotas" } },
  "size": { "px": 152 }
}
```

Это пример замены **только блока quotas** в установленном layout, не всего дерева.
Альтернатива — `QUOTA_URL` в `mods/trin-quotas/transport.js`. Приватные overrides
не коммитить: держите их в отдельном установленном layout/личном config.
Без правок исходников: `herdr-app do trin-quotas.endpoint--<URL-encoded-URL>`
записывает личный endpoint через штатный shared API (`trin.quotas.endpoint`).
URL кодировать `encodeURIComponent` целиком; пример результата:
`herdr-app do trin-quotas.endpoint--https%3A%2F%2Fquota.example.com%2Fv1%2Fquotas`.
`herdr-app do trin-quotas.reset-endpoint` удаляет override, возвращая props/default.
Приоритет: shared override → props.endpoint → QUOTA_URL. Смена адреса очищает
старый кеш и игнорирует поздний ответ прежнего сервиса; токены URL запрещены.
Транспорт принимает точный local default или HTTPS DNS/IPv4 URL `/v1/quotas`
без userinfo/query/fragment. IPv6 URL и произвольные HTTP-порты не поддержаны.
Bearer в текущем UI-транспорте не реализован: для remote сервиса требуется
защищённый частный доступ/reverse proxy; не открытый интернет.

JSON-контракт и синтетический пример: [../../docs/quota-api.md](../../docs/quota-api.md),
`tests/fixtures/quota-v1.json`. Обязательны schema v1, generated_at, subscriptions;
ID уникальны; `null` не равно нулю. Неизвестные тарифы/аккаунты не угадываются.
Email появится только при явном разрешении backend `publish_account_email=true`.

Один GET сразу и раз в 60с, без параллельных запросов; connect timeout 5с, общий
15с, ответ ≤1MiB, без redirects/curlrc. HTTPS проверяет сертификат, HTTP только
на точный loopback endpoint; loopback не проходит через proxy. SSE-контракт квот
не подтверждён, поэтому polling оставлен явно. Кеш в памяти; сбой сохраняет
last-good с предупреждением. Даты карточек в МСК (UTC+3), не timezone компьютера.

## Projects и Agents

Projects хранит реестр через штатный shared API, не исходники. Публичный
`migration.js` пустой: нет чужих машин/путей/ID. Существующие native Spaces
принимаются с их собственными данными; уже сохранённые проекты не перезаписываются.
Форма запускает новые сессии только по подтверждённому действию. Remote directory
browser требует POSIX/Python 3, работает read-only через Herdr exec; Windows SSH
не поддерживается. Смена проекта не перемещает cwd уже работающих процессов.

Agents использует реактивные данные Herdr; расширенная статистика — OpenCode V2
с локальной регистрацией `$XDG_STATE_HOME/opencode/service.json` (default
`~/.local/state/opencode/service.json`) **на каждой owning machine**. Credentials
регистрации не передаются в UI. SSE: snapshot + subscribe-before-reconcile,
bounded lease около 18с, debounce/coalescing до 2с и retry/backoff; 1с таймер —
локальные часы, не upstream polling. Тексты сообщений и полный argv/env в UI не
передаются. Без OpenCode интеграции расширенные поля могут быть недоступны.

## Проверки

Из корня:

```sh
node --test layout/trin/tests/*.test.mjs
python3 -B -m unittest discover -s layout/trin/tests -p 'test_*.py'
herdr-app check "$HOME/.config/herdr-app/mods/trin-quotas"
```

`check` нужен для каждого установленного изменённого мода. Он не заменяет GUI:
проверяйте маленькое окно, длинные названия, rail, темы, Files/Changes, клики и
reconnect. `tests/live-projects.py` — отдельный opt-in smoke с созданием shells;
не запускать автоматически. Native form/detail checks используют временные
пакеты и не доказывают pointer/keyboard поведение.
