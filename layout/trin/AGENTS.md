# Trin layout

Общие правила: `../../CONTRIBUTING.md` (из этой папки).

- Это native GPUI JavaScript, не DOM/Node/web UI. `herdr.d.ts` и `gpui-kit.d.ts`
  установленного приложения — источник API. Встроенные моды только читать.
- Проверять активный layout/ссылки через `herdr-app status`; не затирать symlinks.
- Не править `shared.json`, `view.json`, `layouts/.state/` руками.
- `mods/trin-projects/migration.js` публично содержит только пустые seeds.
- Quotas: default `http://127.0.0.1:8098/v1/quotas`; адрес задаётся в props.endpoint
  блока quotas либо QUOTA_URL в transport.js. Контракт: `../../docs/quota-api.md`.
  Только готовый JSON, без чтения provider credentials. HTTPS для remote, HTTP
  разрешён только для точного local default. URL не содержит auth/query.
  Личный shared override `trin.quotas.endpoint` задаётся командой
  `endpoint--<encodeURIComponent(URL)>`, сбрасывается `reset-endpoint`; выше props.
- Не считать синтетические fixtures реальными аккаунтами. Имена окон/тарифы не
  выводить из предположений; null не равно нулевому расходу.
- После изменения мода: `herdr-app check <каталог>`, offline tests, status/errors,
  визуальная проверка; честно перечислять непроверенные состояния в журнале.
