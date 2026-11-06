# quota-hub

Общие правила: `../CONTRIBUTING.md`. Только stdlib Go, без смены провайдера/SDK
и без LLM-генераций: этот сервис читает usage, не вызывает модели.

- Источники: native Codex OAuth; Claude passive headers + native OAuth fallback;
  Ollama/Kimi — allowlisted активные ключи OpenCode credential API.
- Никогда не обновлять/ротировать credentials, не вызывать auth resolver или
  refresh endpoint. Не копировать БД, service.json, auth.json, сырые ответы в Git.
- OAuth/native metadata остаются на машине владельца. HTTP выдаёт нормализованный
  schema v1 (`../docs/quota-api.md`), email только при publish_account_email=true.
- Политика по умолчанию в коде — Bearer, wildcard listener запрещён. Compose
  явно включает allow_wildcard_listen внутри контейнера и http_auth=none, но
  host port строго 127.0.0.1. Не расширять наружный доступ незаметно.
- Retry-After/backoff/cooldown и last-good переживают restart. GET клиента не
  вызывает upstream polling. Не выдавать stale за fresh, unknown за ноль.
- Все тесты offline через fake HTTP/files/temp dirs, без реальных credentials.
- Документировать ограничения macOS Keychain, Docker networking и token expiry.
