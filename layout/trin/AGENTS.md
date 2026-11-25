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
- Для отображаемых иконок поставщиков: один фирменный SVG (векторный файл)
  на поставщика, общий путь и однотонная окраска `theme().muted` (приглушённый
  цвет темы). Размер можно подстроить под место, сохраняя пропорции. Не заменять
  известный логотип буквой или отдельной версией для другой панели. Для Google
  использовать `mods/trin-agents/model-google.svg`.
  «Сессии» не показывают знак поставщика, но сохраняют отступ названия модели.
  «Подписки» выбирают логотип по поставщику. При его добавлении проверять
  широкие и узкие строки «Подписок» и карточку подробностей.
- После изменения мода: `herdr-app check <каталог>`, offline tests, status/errors,
  визуальная проверка; честно перечислять непроверенные состояния в журнале.
- Projects native mirror — opt-in `trin-projects.enable-native-spaces`:
  `native.js` планирует имена/порядок и фоновые shell Spaces через команды Herdr.
  Не писать core docs вручную и не удалять pending при неизвестном результате.
  Пространства без вкладки API не поддерживает; последний close в mirror-mode
  восстанавливает shell. Удаление проекта/Space — только явное подтверждение.
