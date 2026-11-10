# Passive quota bridge

OpenCode V2 `http.response` plugin: сохраняет только allowlisted quota headers
Anthropic и отказ бесплатных моделей OpenCode Console, без request/response body,
Authorization, cookies или генераций.
Подтверждённый API исходной реализации — OpenCode 2.2.5; при новой версии
проверьте контракт hook. Это не standalone HTTP daemon.

Плагин не устанавливается автоматически. В своём OpenCode config добавьте к
существующему массиву plugins (не заменяйте остальной config):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [{
    "package": "/absolute/path/to/herdr/quota-hub/tools/opencode-bridge",
    "options": {
      "outputFile": "/absolute/path/to/.local/state/quota-hub/bridge/claude.latest.json",
      "freeLimitFile": "/absolute/path/to/.local/state/quota-hub/bridge/opencode.latest.json"
    }
  }]
}
```

Без options default вычисляется от `os.homedir()`:
`~/.local/state/quota-hub/bridge/claude.latest.json` и
`~/.local/state/quota-hub/bridge/opencode.latest.json`, не путь конкретного автора.
Непригодный путь отключает плагин целиком, а не пишет в другое место.
OpenCode подхватывает config без restart. Новая наблюдаемая квота появится только
после обычного ответа модели с нужными headers; установка плагина ничего не генерирует.

Latest observation (Claude): schema_version=1, source=`opencode-http-response-hook`,
provider_id=`anthropic`, observed_at, endpoint, status и quota[] с name/type/family/value.
Latest observation (Console): schema_version=1, source=`opencode-http-response-hook`,
provider_id=`opencode`, observed_at, endpoint, status=429 и одна запись
`free_limit_reset` — момент, когда дневной лимит бесплатных моделей вернётся.
Writes: directory 0700, file 0600, temp + rename; один writer, newest wins,
ограниченный pending, fail-open, без timers/network/body logging. Отсутствие
quota headers не затирает last-good. Staleness определяет Go reader.

Для Compose отдельно подключите directory bridge в `/bridge:ro` через локальный
override (пример в `../../README.md`). В default Compose bridge отсутствует:
работает native Claude fallback, если доступен файловый credential.

```sh
node --test quota-hub/tools/opencode-bridge/test/*.test.mjs
```
