# Локальная выкладка учёта токенов

`quota-usage` — импортёр, `tools/opencode-usage-bridge` — плагин OpenCode. Один коммит
на комплект, без npm. Каталог данных `~/.local/state/quota-usage/` не переносится и не
удаляется ни при обновлении, ни при откате.

## 1. Подготовка

Отдельная ветка, ревью, чистый checkout выбранного коммита,
[офлайн-проверки](README.md#offline-проверки) и [чек-лист безопасности](../docs/security.md).

## 2. Сборка

Из `quota-hub/`:

```bash
rev=$(git rev-parse --short=12 HEAD)
out="$HOME/.local/share/quota-hub/releases/$rev"
mkdir -p "$out/opencode-usage-bridge"
CGO_ENABLED=0 go build -trimpath -o "$out/quota-usage" ./cmd/quota-usage
git -C .. archive HEAD:quota-hub/tools/opencode-usage-bridge package.json server.ts bridge.mjs ratelimit.mjs \
  | tar -x -C "$out/opencode-usage-bridge"
```

## 3. Выкладка

1. Unit `~/.config/systemd/user/quota-usage.service`:
   `ExecStart=<out>/quota-usage`, `Restart=on-failure`, `UMask=0077`.
   `systemctl --user daemon-reload && systemctl --user restart quota-usage`.
2. В `~/.config/opencode/opencode.jsonc` в `plugins` указать `<out>/opencode-usage-bridge`
   вместо прежнего пути. OpenCode сам перечитывает конфиг; перезапуск не нужен.
3. Проверить: `opencode plugin list` показывает новый путь, плагин активен; после
   ответа модели в `events/` появилась строка; `quota-usage -summary` свежая, `ok: true`.

Порядок: сначала один пользователь, затем остальные.

## 4. Откат

Вернуть прежний путь плагина в `opencode.jsonc` и прежний `ExecStart`, затем
`daemon-reload` и `restart`. Данные не трогать: старая и новая версии пишут
одинаковый формат, дубли убирает импортёр.
