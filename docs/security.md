# Публичный набор и приватные данные

## Что остаётся локально

Корневой `.gitignore` default-deny: корневые личные AGENTS/CHANGELOG, `.local/`,
credentials и generated types не входят в Git. Устаревшие промпты, галереи
макетов, пустые моды и корневые снимки удалены; ignore-правила оставлены против
случайного повторного добавления. Копии старых персональных документов сохранены локально
в `.local/publication-backup/`, не в публичной истории.

`.env`, `*.local.*`, credentials/auth/service registration, private DB/state,
лог и raw API output не являются source code. Новые файлы внутри разрешённых
директорий всё равно требуют review. Для публичной истории начинаем новый Git,
не импортируем историю quota-hub с личными deployment-материалами.

## Перед публикацией

1. `python3 scripts/check-public.py`: проверяет конкретные unignored/ tracked
   файлы, symlinks, типичные secrets, личные пути/адреса и непереносимый tooling. Значения
   совпадений не выводятся. Это эвристика, не доказательство отсутствия секретов.
2. Обязателен независимый secret scanner, например установленный `gitleaks`:
   `gitleaks dir <чистый checkout> --redact`. Не сканируйте только текущий diff.
   Если есть commits, дополнительно `gitleaks git . --redact` для истории.
3. Просмотреть `git status --short --untracked-files=all` и весь staged diff.
   Не использовать `git add -f` и не загружать ZIP всей рабочей папки.
4. Тестировать именно чистый Git-набор: отсутствие прототипов/generated types не
   должно ломать тесты/установку. Не запускать live tests автоматически.
5. Сохранять LICENSE/THIRD_PARTY и отдельные notices: собственный код MIT,
   оригинальные части Herdr Apache-2.0; владелец разрешил Deliva/logo только в
   составе layout, без передачи trademark rights. Вся монорепа не «целиком MIT».
6. Только затем commit/remote/push/Release ZIP. ZIP лучше из `git archive`,
   а не Finder «Сжать папку». Проверять также содержимое будущего архива.

## Runtime

Loopback API не является общедоступным, но доступно локальным процессам;
контейнерный listener также доступен внутри Compose-сети. Default без email.
Нельзя публиковать port без firewall/auth/TLS. Mounts read-only не означают
«контейнер не может прочесть лишнее»: ему доступны два смонтированных каталога.
Не экспортируйте macOS Keychain в repo. Не меняйте права credentials ради Docker.
Сервис не делает refresh, не меняет вход и не гарантирует безопасность приватных
upstream endpoints/отсутствие rate limits: соблюдайте условия провайдеров.

Backend перенесён как новые исходники, не полный аудит безопасности каждого
алгоритма. Security review исходников/fixtures и опубликованного набора обязателен.
