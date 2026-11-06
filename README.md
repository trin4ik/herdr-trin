# Herdr Trin + quota-hub

GitHub: [trin4ik/herdr-trin](https://github.com/trin4ik/herdr-trin).

Одна монорепа: нативный layout Herdr с Projects, Agents, вкладками и подписками,
плюс необязательный Go-сервис квот. Не исходники самого Herdr.

```text
layout/trin/       layout, собственные GPUI-моды, keys и тесты
quota-hub/         Go-сборщик, HTTP API и пассивный Claude bridge
compose.yaml      локальный сервис квот
docs/             контракт, безопасность, публичный журнал
scripts/          установка и проверка публичного набора
```

## Быстрый запуск квот

Требуется Docker с Compose v2. После входа в родные Codex/Claude CLI:

```sh
docker compose config --quiet
docker compose up -d --build
curl --noproxy '*' http://127.0.0.1:8098/v1/quotas
```

Если Compose установлен отдельно, используйте `docker-compose` вместо
`docker compose`. Перед запуском проверьте `docker context show`: daemon должен
быть локальным и иметь доступ к вашим credential-каталогам.

Compose читает **только read-only каталоги** `${HOME}/.codex` и `${HOME}/.claude`;
они должны существовать. Пути переопределяются через `.env` (см. `.env.example`).
В каталоге Codex ожидается `auth.json`, Claude — `.credentials.json`.
Read-only mount каталога нужен для подхвата атомарной замены токенов CLI; контейнер
при этом может читать и другие файлы этих двух каталогов. Весь home не монтируется.

**Это не универсальный auto-login.** Если native credential-файла нет (например,
Claude хранит вход только в macOS Keychain), соответствующая подписка будет
unavailable. Keychain не экспортируется автоматически. Сервис не обновляет OAuth,
не выполняет генераций и не меняет вход CLI. Истёкший вход обновляет владелец через
родной CLI. Ollama/Kimi по умолчанию выключены: им нужен доступ к credential API
работающего OpenCode, а не копия его БД. См. [quota-hub](quota-hub/README.md).

API публикуется **только на 127.0.0.1:8098**, без Bearer; email выключен.
Он доступен локальным процессам и контейнерам той же Compose-сети. Не меняйте
bind на `0.0.0.0` и не публикуйте API в интернет без отдельной защиты.
Сервис хранит private state/history в volume `quota-state`; автоматической
ротации истории пока нет. `docker compose down` сохраняет volume.

## Установка интерфейса

Сначала прочитайте [layout/trin/README.md](layout/trin/README.md).

```sh
python3 scripts/install-layout.py --config-dir "$HOME/.config/herdr-app"
herdr-app do layout.switch '{"name":"trin"}'
```

Установщик отказывается перезаписывать существующие layout, моды, assets или
секцию `layout:trin`; существующие настройки/сессии не переносит. Сам layout не
переключает. При установке используется копирование: для разработки настройте
symlink-моды отдельно, а изображения оставьте реальными копиями в `assets/`.

Мод подписок по умолчанию читает `http://127.0.0.1:8098/v1/quotas` на компьютере
Herdr, даже если выбран удалённый проект. Для своего HTTPS-сервиса задайте
`content.props.endpoint` у блока `quotas` в установленном `layouts/trin.json`;
либо измените `QUOTA_URL` в `mods/trin-quotas/transport.js`. Не вставляйте токены
в URL. Точный ответ сервиса: [docs/quota-api.md](docs/quota-api.md).

Чтобы не править tracked layout, есть личный runtime override через команду
`trin-quotas.endpoint--<encodeURIComponent(URL)>`; он хранится штатным Herdr
shared API вне репозитория. `trin-quotas.reset-endpoint` возвращает props/default.
Личный override имеет приоритет над props и не переносится другим пользователям.

## Разработка без двух копий исходников

Правим только `layout/trin/` и `quota-hub/` здесь. Корневой `.gitignore` по умолчанию
скрывает личные файлы: исследования, старые снимки, локальный корневой AGENTS и
исторический CHANGELOG не публикуются. `.env` и `*.local.*` тоже игнорируются.
Публичные инструкции лежат в `layout/trin/AGENTS.md`, `quota-hub/AGENTS.md` и
[CONTRIBUTING.md](CONTRIBUTING.md). Игнорирование **не защищает** уже tracked
файлы, содержимое разрешённых исходников, `git add -f` или архив всей папки.

Старые галереи макетов, корневые скриншоты, выполненные task-промпты и пустые
заготовки модов удалены, а не только скрыты из Git. Регрессионные тесты и
используемые runtime-ресурсы сохранены.

### Требования внешней установки

- Layout: установленный Herdr (`herdr-app` и его core `hc`), curl; Python 3.9+
  для удалённого выбора директорий и расширенной статистики Agents.
- OpenCode V2 на отслеживаемой машине — только для расширенных данных Agents и
  источников квот, которым он нужен; приватная платформа/служба запуска не нужна.
- Backend: Docker + Compose либо Go 1.27+ для native-сборки. Собственные входы
  провайдеров и ограничения credentials описаны выше и в quota-hub/README.md.
- Разработка: Git, Node для JS-тестов, Go и Python. SSH нужен только opt-in
  remote diagnostics; они не выполняются при установке или запуске тестового suite.

Runtime-команды активных модов ограничены штатными `herdr-app`, `curl`, `python3`;
старый неподключённый Spaces использует стандартные Git и Herdr core `hc`. Пользовательских wrapper CLI,
локальных task runners и приватных путей к инструментам нет.

Перед добавлением и перед push проверяйте конкретный набор файлов:

```sh
python3 scripts/check-public.py
git status --short --untracked-files=all
```

Подробности: [docs/security.md](docs/security.md). Подготовка не означает публикацию:
push и deploy выполняются только по отдельному решению владельца.
Собственный код — [MIT](LICENSE); исходные части Herdr — Apache-2.0, сторонние
иконки и фирменная графика имеют отдельные условия: [THIRD_PARTY.md](THIRD_PARTY.md).
