#!/usr/bin/env bash
# Blue/green deploy for one server (spec 9.5).
#
#   deploy.sh /opt/cms/releases/<version>        # new core release
#   deploy.sh --site /opt/cms/site/releases/<id> # new site bundle
#
# Several sites on one server (cms install --name): pass --name first.
#
#   deploy.sh --name docs /opt/cms/releases/<version>
#   deploy.sh --name docs --site /opt/cms/sites/docs/site/releases/<id>
#
# A named site lives in /opt/cms/sites/<name>, runs as cms-blue@<name> /
# cms-green@<name> on the ports in /etc/cms/<name>/{blue,green}.env, and sits
# behind nginx: switching rewrites /etc/nginx/cms/<name>-upstream.conf and
# reloads nginx. The unnamed layout is unchanged (Caddy, cms@blue).
#
# The running colour keeps serving until the new one passes /readyz,
# `cms doctor` and a watch period. Any failure leaves the old colour serving.
set -euo pipefail

NAME=
if [[ ${1:-} == --name ]]; then NAME=${2:?--name needs a site name}; shift 2; fi
WATCH_SECONDS=${WATCH_SECONDS:-300}
if [[ -z $NAME ]]; then
  CMS_HOME=/opt/cms
  CONFIG=/etc/cms/config.yaml
  UPSTREAM=/etc/caddy/cms-upstream.caddy
  SITE_CADDY=/etc/caddy/sites/cms.caddy
  declare -A PORT=([blue]=8081 [green]=8082)
  LOCK=/run/cms-deploy.lock
  unit() { echo "cms@$1"; }
  WORKER=cms-worker
else
  [[ $NAME =~ ^[a-z][a-z0-9-]{0,30}[a-z0-9]$ ]] || { echo "deploy: bad site name $NAME" >&2; exit 1; }
  CMS_HOME=/opt/cms/sites/$NAME
  CONFIG=/etc/cms/$NAME/config.yaml
  UPSTREAM=/etc/nginx/cms/$NAME-upstream.conf
  declare -A PORT
  for c in blue green; do
    PORT[$c]=$(sed -n 's/^PORT=\([0-9]*\)$/\1/p' "/etc/cms/$NAME/$c.env")
    [[ -n ${PORT[$c]} ]] || { echo "deploy: no PORT in /etc/cms/$NAME/$c.env" >&2; exit 1; }
  done
  LOCK=/run/cms-deploy-$NAME.lock
  unit() { echo "cms-$1@$NAME"; }
  WORKER=cms-worker@$NAME
fi

log() { printf '%s deploy: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { log "FAILED: $*"; exit 1; }

exec 9>"$LOCK"
flock -n 9 || die "another deploy is running"

mode=core
if [[ ${1:-} == --site ]]; then mode=site; shift; fi
target=${1:?usage: deploy.sh [--name <site>] [--site] <release-dir>}
[[ -d $target ]] || die "$target is not a directory"

active=$(cat "$CMS_HOME/active-colour" 2>/dev/null || echo blue)
idle=green; [[ $active == green ]] && idle=blue
log "active=$active idle=$idle mode=$mode target=$target"

if [[ $mode == core ]]; then
  [[ -x $target/cms ]] || die "$target/cms is missing or not executable"
  new_bin=$target/cms
  ln -sfn "$target" "$CMS_HOME/$idle"
else
  # A new site bundle keeps the core release: point the idle colour at the
  # release folder itself. Linking it to the active colour's link made a
  # loop on the second site deploy in a row (green -> blue -> green).
  core=$(readlink -f "$CMS_HOME/$active")
  [[ -x $core/cms ]] || die "the active colour ($active) doesn't resolve to a release with cms in it"
  new_bin=$core/cms
  ln -sfn "$core" "$CMS_HOME/$idle"
fi
# One-off commands run as cms, like the services, and get the key the same
# way (LoadCredential): the key file itself is root-only (0400), so a plain
# `sudo -u cms` can't read it and anything that needs it (schema apply)
# fails with "permission denied".
KEK=$(dirname "$CONFIG")/kek
cms() {
  if [[ -f $KEK ]]; then
    systemd-run --quiet --pipe --wait --collect --uid=cms --gid=cms \
      -p LoadCredential=kek:"$KEK" "$new_bin" --config "$CONFIG" "$@"
  else
    sudo -u cms "$new_bin" --config "$CONFIG" "$@"
  fi
}

log "pre-migration backup"
cms backup create --keep 14 -q || die "backup failed: refusing to migrate without one"

if [[ $mode == site ]]; then
  prev_site=$(readlink "$CMS_HOME/site/current" || true)
  ln -sfn "$target" "$CMS_HOME/site/current"
fi

log "migrations (expand only)"
cms migrate up -q || die "migration failed (the active colour is untouched)"
cms schema apply -q || { [[ $mode == site && -n ${prev_site:-} ]] && ln -sfn "$prev_site" "$CMS_HOME/site/current"; die "schema apply failed"; }

log "starting $(unit "$idle") on :${PORT[$idle]}"
systemctl restart "$(unit "$idle")"
for i in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${PORT[$idle]}/readyz" >/dev/null 2>&1; then break; fi
  sleep 1
  [[ $i == 60 ]] && { systemctl stop "$(unit "$idle")"; die "$(unit "$idle") never became ready"; }
done
doctor_out=$(mktemp)
cms doctor --json >"$doctor_out" || { systemctl stop "$(unit "$idle")"; die "cms doctor failed: $(cat "$doctor_out")"; }

switch_to() {
  if [[ -z $NAME ]]; then
    printf 'reverse_proxy 127.0.0.1:%s {\n\thealth_uri /readyz\n\thealth_interval 10s\n}\n' "${PORT[$1]}" >"$UPSTREAM.tmp"
    mv "$UPSTREAM.tmp" "$UPSTREAM"
    cms caddy >"$SITE_CADDY.tmp" && mv "$SITE_CADDY.tmp" "$SITE_CADDY"
    systemctl reload caddy
  else
    # The server block (cms nginx) stays as it is: only the upstream moves.
    local prev
    prev=$(cat "$UPSTREAM" 2>/dev/null || true)
    printf 'proxy_pass http://127.0.0.1:%s;\n' "${PORT[$1]}" >"$UPSTREAM.tmp"
    mv "$UPSTREAM.tmp" "$UPSTREAM"
    if ! nginx -t -q; then
      printf '%s\n' "$prev" >"$UPSTREAM"
      die "nginx -t failed after switching the upstream; restored it"
    fi
    systemctl reload nginx
  fi
}

# Pages are cached as rendered. After a switch, the version now serving
# renders them again, so a theme or template change shows on every page at
# once, not as each page happens to be edited. A failed rebuild only warns:
# the site is up, and pages still re-render as they change.
rebuild_cache() {
  local saved=$new_bin
  new_bin=$1
  log "rebuilding the page cache"
  cms cache rebuild -q || log "warning: the page cache rebuild failed"
  new_bin=$saved
}

log "switching traffic to $idle"
switch_to "$idle"
rebuild_cache "$new_bin"

log "watching for ${WATCH_SECONDS}s"
host=$(cms config show | sed -n 's/.*"BaseURL": "\(.*\)".*/\1/p' | head -1)
fails=0
end=$((SECONDS + WATCH_SECONDS))
while (( SECONDS < end )); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$host/" || echo 000)
  ready=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${PORT[$idle]}/readyz" || echo 000)
  if [[ $code != 200 || $ready != 200 ]]; then
    fails=$((fails + 1))
    log "check failed (site=$code ready=$ready, $fails/3)"
    if (( fails >= 3 )); then
      log "rolling back to $active"
      switch_to "$active"
      systemctl stop "$(unit "$idle")"
      [[ $mode == site && -n ${prev_site:-} ]] && ln -sfn "$prev_site" "$CMS_HOME/site/current"
      rebuild_cache "$CMS_HOME/$active/cms"
      die "health checks failed after the switch; $active is serving again"
    fi
  fi
  sleep 10
done

log "stopping $(unit "$active") (graceful drain)"
systemctl stop "$(unit "$active")"
# The live colour is the one that comes back after a reboot.
systemctl enable --quiet "$(unit "$idle")"
systemctl disable --quiet "$(unit "$active")"
echo "$idle" >"$CMS_HOME/active-colour"
ln -sfn "$CMS_HOME/$idle" "$CMS_HOME/current"
log "restarting the worker"
systemctl restart "$WORKER"
log "done: $idle is live"
