#!/bin/bash
# Read-only views of the slot's test hub (e2e/README.md › Observe). Opens the hub DB read-only; never writes.
#   scripts/observe.sh [--slot N] owner    `user_id chat_id first_name paired_at`, or `none`
#   scripts/observe.sh [--slot N] events   seq created kind status chat msg error, oldest first
#   scripts/observe.sh [--slot N] jobs     created id type status target host=<host it ran on> result, oldest first
#   scripts/observe.sh [--slot N] hosts    name online last_seen version checks (GET /api/status, the host key)
#   scripts/observe.sh [--slot N] pings    created msg slug host session, oldest first
#   scripts/observe.sh [--slot N] prompts  created id type msg prompt (first 120 chars, newlines as ⏎) of each run job
# Tables print a header line, then one row per record (`none` when empty). Exit 1: no hub DB or hub not answering.
set -euo pipefail
HM_ME=observe
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,9s/^# \{0,1\}//p' "$0" >&2; exit 2; }
slot=$(hm_default_slot)
[[ ${1:-} == --slot ]] && { slot=${2:?--slot needs a number}; shift 2; }
hm_check_slot "$slot"
view=${1:-}
[[ -n $view ]] || usage
root=$(hm_root "$slot")
db=$root/home/.local/state/hommy-$(hm_instance "$slot")/hub/hub.sqlite3

q() { # SQL: tab-separated rows with a header, `none` when there are none
  [[ -f $db ]] || hm_die "no hub DB at $db (is the test hub installed?)"
  local out
  out=$(/usr/bin/sqlite3 -readonly -header -separator $'\t' "file:$db?mode=ro" "$1") || hm_die "query failed"
  if [[ $(printf '%s\n' "$out" | grep -c .) -le 1 ]]; then echo none; else printf '%s\n' "$out"; fi
}

ts() { echo "strftime('%H:%M:%S', $1, 'unixepoch', 'localtime')"; }

case $view in
  owner)
    [[ -f $db ]] || hm_die "no hub DB at $db (is the test hub installed?)"
    sql="SELECT user_id, chat_id, first_name, strftime('%Y-%m-%dT%H:%M:%S', paired_at, 'unixepoch', 'localtime')
         FROM owner LIMIT 1"
    out=$(/usr/bin/sqlite3 -readonly -separator ' ' "file:$db?mode=ro" "$sql") || hm_die "query failed"
    echo "${out:-none}" ;;
  events)
    q "SELECT seq, $(ts created_at) AS created, kind, status, chat_id AS chat, msg_id AS msg, substr(error, 1, 60) AS error
       FROM events ORDER BY seq" ;;
  jobs)
    q "SELECT $(ts created_at) AS created, id, type, status, target, 'host=' || host AS host,
       substr(replace(result, char(10), ' '), 1, 60) AS result FROM jobs ORDER BY created_at, seq, sub" ;;
  prompts)
    q "SELECT $(ts created_at) AS created, id, type, msg_id AS msg,
       substr(replace(json_extract(payload, '$.prompt'), char(10), '⏎'), 1, 120) AS prompt
       FROM jobs WHERE json_extract(payload, '$.prompt') IS NOT NULL ORDER BY created_at, seq, sub" ;;
  pings)
    q "SELECT $(ts created_at) AS created, msg_id AS msg, slug, host, session FROM pings ORDER BY created_at" ;;
  hosts)
    port=$(hm_base_port "$slot")
    name=$("$HM_REPO/scripts/hm.sh" --slot "$slot" config get HOST_NAME) || hm_die "cannot read HOST_NAME"
    key=$("$HM_REPO/scripts/test-env.sh" host-key --slot "$slot") || exit 1
    body=$(curl -fsS -m 10 -H "Authorization: Bearer $key" -H "X-Hommy-Host: $name" \
      "http://127.0.0.1:$port/api/status") || hm_die "GET /api/status failed on :$port"
    unset key
    printf 'name\tonline\tlast_seen\tversion\tchecks\n'
    jq -r '.hosts[] | [.name, (.online | tostring), (.last_seen // "" | tostring), (.version // ""),
      ((.checks // {}) | tojson)] | @tsv' <<<"$body" ;;
  *) usage ;;
esac
