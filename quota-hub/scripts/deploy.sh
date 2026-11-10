#!/usr/bin/env bash
# Update the running quota-hub from its checkout.
#
# The deployment directory is a checkout of the public repository. The update is
# three steps: pull the verified source, rebuild the binary, restart the service
# and check that it became ready. A failed check restores the previous binary.
#
#   SERVICE      systemd user unit to restart              (quota-hub)
#   HEALTH_URL   readiness endpoint of the running service  (required)
#   GO           go binary to build with                    (go, or ~/sdk/go/bin/go)
#   SKIP_PULL    set to 1 to rebuild the current source
#
# The script is generic: no host, no path and no secret of this machine is
# hardcoded, so the same file works for any deployment.
set -euo pipefail

# bash reads a script by byte offset: a pull may rewrite this very file while it
# runs. Re-execute from a copy that is already fully read.
if [ -z "${QUOTA_HUB_DEPLOY_REEXEC:-}" ]; then
  export QUOTA_HUB_DEPLOY_REEXEC=1
  export QUOTA_HUB_DEPLOY_SCRIPT="$(cd -- "$(dirname -- "$0")" && pwd)/$(basename -- "$0")"
  exec bash <(cat "$0") "$@"
fi

SERVICE=${SERVICE:-quota-hub}
HEALTH_URL=${HEALTH_URL:-}
SKIP_PULL=${SKIP_PULL:-0}

log() { printf '%s %s\n' "[deploy]" "$*"; }
fail() { printf '%s %s\n' "[deploy] ошибка:" "$*" >&2; exit 1; }

[ -n "$HEALTH_URL" ] || fail "HEALTH_URL не задан"

here=$(cd -- "$(dirname -- "${QUOTA_HUB_DEPLOY_SCRIPT:-$0}")" && pwd)
package=$(dirname -- "$here")   # .../quota-hub
root=$(dirname -- "$package")   # .../<repository>

GO=${GO:-go}
command -v "$GO" >/dev/null 2>&1 || { [ -x "$HOME/sdk/go/bin/go" ] && GO="$HOME/sdk/go/bin/go"; }
command -v "$GO" >/dev/null 2>&1 || fail "не найден go (укажите GO=...)"

if [ "$SKIP_PULL" != "1" ]; then
  log "обновляю исходники в $root"
  git -C "$root" pull --ff-only || fail "git pull не удался"
fi

log "собираю $package"
(cd "$package" && CGO_ENABLED=0 "$GO" build -trimpath -ldflags="-s -w" -o bin/quota-hub.new ./cmd/quota-hub) \
  || fail "сборка не удалась"

[ -x "$package/bin/quota-hub" ] && cp -p "$package/bin/quota-hub" "$package/bin/quota-hub.prev"
mv -f "$package/bin/quota-hub.new" "$package/bin/quota-hub"
log "бинарник собран"

systemctl --user restart "$SERVICE" || fail "не перезапустился $SERVICE"

ready=0
for _ in $(seq 1 30); do
  if curl --fail --silent --show-error --max-time 5 -o /dev/null "$HEALTH_URL"; then ready=1; break; fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  log "сервис не стал готовым, откат на предыдущий бинарник"
  if [ -x "$package/bin/quota-hub.prev" ]; then
    cp -p "$package/bin/quota-hub.prev" "$package/bin/quota-hub"
    systemctl --user restart "$SERVICE" || true
  fi
  fail "откат выполнен, разберитесь с состоянием сервиса"
fi

log "сервис $SERVICE готов: $(systemctl --user is-active "$SERVICE")"
