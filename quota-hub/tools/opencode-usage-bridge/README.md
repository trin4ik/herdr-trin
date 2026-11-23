# Учёт токенов OpenCode

Пишет вызовы `session.step.ended/failed` с токенами и `session.usage.recorded`
в `D/events/YYYY-MM-DD.jsonl` (дата события, UTC).
`D` — абсолютный `$XDG_STATE_HOME/quota-usage`, иначе `$HOME/.local/state/quota-usage`.
Новые каталоги имеют режим 0700, файлы — 0600.

Каждая строка — JSON v1 с завершающим переводом строки:

```json
{"v":1,"id":"evt_example","time":1000,"source":"step","status":"ended","session":"ses_example","message":"msg_example","provider":"example","model":"example","variant":null,"agent":"build","tokens":{"input":10,"output":4,"reasoning":2,"cacheRead":3,"cacheWrite":2},"cost":0}
```

Модель берётся из парного `session.step.started`; без него поля модели — `null`.
Некорректные события и события без токенов пропускаются. Заголовки и сжатие
(`session.usage.recorded`) OpenCode 2.3 плагинам не отдаёт, вызовы `generate` событий
не имеют — такой расход не попадает в учёт.
`cost` — цена OpenCode, не расчёт плагина.

OpenCode запускает экземпляр плагина на каждый location, и каждый получает все события
процесса. Экземпляры (и версии) делят только список уже взятых `id` событий, поэтому
строка пишется один раз. Оставшиеся повторы (например, при смене версии) удаляет импортёр.

## Лимиты Claude из ответов

Провайдер OpenCode `claude-agent` (Claude Code harness) передаёт `rate_limit_event`
каждого ответа Claude в `native.rateLimit` события `session.step.ended/failed`.
Плагин переводит `unifiedWindows` (`five_hour`, `seven_day`, `seven_day_<модель>`)
в файл наблюдения quota-hub `~/.local/state/quota-hub/bridge/claude.latest.json`
(`source: opencode-claude-agent`, тот же формат, что у header bridge). Запись
атомарная (temp + rename), каталог 0700, файл 0600, более старый ответ не
перезаписывает более новый. Запросов к Anthropic нет: нет ответов — нет обновлений.

Опции: `claudeQuotaFile` — абсолютный путь файла; `claudeProviders` — список ID
провайдеров (по умолчанию `["claude-agent"]`). Аккаунт в событии не указан:
все перечисленные провайдеры должны работать на одном аккаунте Claude, иначе
снимки разных аккаунтов перепишут друг друга.

Подключение: добавить абсолютный путь к каталогу пакета в `plugins` в `opencode.jsonc`:

```jsonc
{
  "plugins": ["/absolute/path/to/opencode-usage-bridge"],
}
```

Опция `directory` задаёт абсолютный путь непосредственно к каталогу событий;
прочие опции игнорируются. Зависимостей и установки через npm нет.

Аккаунт вызова в событиях отсутствует.
Ошибки записи не останавливают OpenCode; потерянные при ошибке строки не повторяются.
Без fsync — при падении ОС могут пропасть последние секунды.
Cleanup ждёт запись не более 2 секунд.

Проверка: `node --test "test/*.test.mjs"`.
