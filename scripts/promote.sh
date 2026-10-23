#!/usr/bin/env bash
# Release rings (spec 9.4). Each release notes body carries "channel:<name>".
#   edge   -> canary after 24 h on staging with /readyz healthy and that version live
#   canary -> stable after 72 h on your own site, same checks
# Manual: promote.sh --tag v1.2.3 --channel stable
set -euo pipefail
set_channel() {
  local tag=$1 channel=$2 body
  body=$(gh release view "$tag" --json body -q .body | sed -E 's/channel:(edge|canary|stable)//g')
  gh release edit "$tag" --notes "$(printf '%s\n\nchannel:%s\n' "$body" "$channel")"
  echo "$tag -> $channel"
}
if [[ ${1:-} == --tag ]]; then
  set_channel "$2" "$4"
  exit 0
fi
healthy_on() { # url tag
  [[ -n $1 ]] || return 1
  local v
  v=$(curl -fsS --max-time 10 "$1/readyz" | jq -r .version 2>/dev/null) || return 1
  [[ $v == "${2#v}" || $v == "$2" ]]
}
now=$(date -u +%s)
gh release list --limit 20 --json tagName,publishedAt,isPrerelease -q '.[] | select(.isPrerelease|not) | "\(.tagName) \(.publishedAt)"' |
while read -r tag published; do
  age=$(( now - $(date -u -d "$published" +%s) ))
  body=$(gh release view "$tag" --json body -q .body)
  if grep -q 'channel:edge' <<<"$body" && (( age > 86400 )) && healthy_on "${STAGING_URL:-}" "$tag"; then
    set_channel "$tag" canary
  elif grep -q 'channel:canary' <<<"$body" && (( age > 4 * 86400 )) && healthy_on "${CANARY_URL:-}" "$tag"; then
    set_channel "$tag" stable
  fi
done
