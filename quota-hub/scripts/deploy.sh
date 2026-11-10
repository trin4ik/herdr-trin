#!/usr/bin/env bash
# Deploy from the public monorepo. See quota-hub/README.md for requirements.
set -euo pipefail

# The checkout may update this script during git pull. Run a snapshot instead.
if [ -z "${QUOTA_HUB_DEPLOY_REEXEC:-}" ]; then
  script=$(readlink -f -- "$0") || exit 1
  export QUOTA_HUB_DEPLOY_REEXEC=1 QUOTA_HUB_DEPLOY_SCRIPT="$script"
  exec bash <(cat -- "$script") "$@"
fi

SERVICE=${SERVICE:-quota-hub}
HEALTH_URL=${HEALTH_URL:-}
CONFIG=${CONFIG:-}
SKIP_PULL=${SKIP_PULL:-0}
GO=${GO:-go}
log() { printf '[deploy] %s\n' "$*"; }
fail() { printf '[deploy] ошибка: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || fail "требуется Linux с user systemd"
for tool in git systemctl curl flock mktemp readlink; do
  command -v "$tool" >/dev/null 2>&1 || fail "не найден $tool"
done
if ! command -v "$GO" >/dev/null 2>&1 && [ -x "$HOME/sdk/go/bin/go" ]; then GO="$HOME/sdk/go/bin/go"; fi
command -v "$GO" >/dev/null 2>&1 || fail "не найден go (укажите GO=...)"
case "$CONFIG" in /*) [ -f "$CONFIG" ] && [ -r "$CONFIG" ] || fail "CONFIG недоступен" ;; *) fail "CONFIG должен быть абсолютным путём к конфигу сервиса" ;; esac
case "$HEALTH_URL" in http://*/readyz|https://*/readyz) ;; *) fail "HEALTH_URL должен указывать на /readyz" ;; esac
[ "$(systemctl --user show "$SERVICE" --property=LoadState --value)" = loaded ] || fail "user unit недоступен или не загружен"

here=$(cd -P -- "$(dirname -- "$QUOTA_HUB_DEPLOY_SCRIPT")" && pwd -P)
package=$(cd -P -- "$here/.." && pwd -P)
root=$(cd -P -- "$package/.." && pwd -P)
git_root=$(git -C "$package" rev-parse --show-toplevel) || fail "не найден Git checkout"
[ "$(cd -P -- "$git_root" && pwd -P)" = "$root" ] && [ -f "$package/go.mod" ] && [ "$(git -C "$package" rev-parse --show-prefix)" = "quota-hub/" ] \
  || fail "ожидался quota-hub в корне монорепозитория"

mkdir -p "$package/bin"
exec 9>"$package/bin/.deploy.lock"
flock -n 9 || fail "другой deploy уже выполняется"

if [ "$SKIP_PULL" != 1 ]; then
  log "обновляю исходники в $root"
  git -C "$root" pull --ff-only || fail "git pull не удался"
fi

work=$(mktemp -d "$package/bin/.deploy.XXXXXXXX")
keep_work=0
cleanup() { if [ "$keep_work" = 0 ]; then rm -rf -- "$work"; else log "файлы неудачного отката сохранены: $work"; fi; }
trap cleanup EXIT
log "собираю и проверяю конфигурацию"
(cd "$package" && CGO_ENABLED=0 "$GO" build -trimpath -ldflags="-s -w" -o "$work/candidate" ./cmd/quota-hub) || fail "сборка не удалась"
"$work/candidate" -config "$CONFIG" -check-config >/dev/null 2>&1 || fail "конфигурация не подходит новому бинарнику"

previous=0
if [ -f "$package/bin/quota-hub" ] && [ -x "$package/bin/quota-hub" ]; then
  cp -p -- "$package/bin/quota-hub" "$work/previous" || fail "не удалось сохранить предыдущий бинарник"
  previous=1
fi

ready() {
  local code i
  for ((i=0; i<30; i++)); do
    code=$(curl --disable --noproxy '*' --proto '=http,https' --silent --max-time 5 -o /dev/null -w '%{http_code}' "$HEALTH_URL") || code=000
    if [ "$code" = 200 ] && systemctl --user is-active --quiet "$SERVICE"; then return 0; fi
    sleep 1
  done
  return 1
}

rollback() {
  log "развёртывание не удалось; проверяю возможность отката бинарника"
  if [ "$previous" != 1 ]; then
    systemctl --user stop "$SERVICE" || log "не удалось остановить сервис"
    log "предыдущего бинарника нет; откат невозможен"
    return 1
  fi
  if ! cp -p -- "$work/previous" "$work/restore" || ! mv -f -- "$work/restore" "$package/bin/quota-hub"; then
    keep_work=1
    log "не удалось восстановить бинарник"
    return 1
  fi
  if ! systemctl --user restart "$SERVICE" || ! ready; then
    keep_work=1
    log "предыдущий бинарник восстановлен, но сервис не прошёл проверку"
    return 1
  fi
  log "предыдущий бинарник восстановлен и сервис готов; исходники, конфиг и state не откатывались"
  return 0
}

mv -f -- "$work/candidate" "$package/bin/quota-hub" || fail "не удалось установить бинарник"
if ! systemctl --user restart "$SERVICE"; then
  rollback || true
  fail "перезапуск нового бинарника не удался"
fi
if ! ready; then
  rollback || true
  fail "новый бинарник не прошёл readiness"
fi
log "сервис $SERVICE активен и готов"
