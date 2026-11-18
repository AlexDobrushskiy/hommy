#!/bin/bash
# The booking fixture site "Barbearia Exemplo" (hommy-e2e booking-site) on the slot's port B+5 (docs/testing.md §2),
# detached, through scripts/sx.sh. Its request log: /tmp/hm-s<N>/logs/booking.jsonl (one JSON line per request).
#   scripts/booking.sh [--slot N] up [--today YYYY-MM-DD]   start it (idempotent); prints the URL
#   scripts/booking.sh [--slot N] down                      stop it
#   scripts/booking.sh [--slot N] log                       the request log
#   scripts/booking.sh [--slot N] reset                     empty the log and forget the bookings
#   scripts/booking.sh [--slot N] bookings                  the recorded bookings (JSON)
#   scripts/booking.sh [--slot N] url | status
set -euo pipefail
HM_ME=booking
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,9s/^# \{0,1\}//p' "$0" >&2; exit 2; }
slot=$(hm_default_slot)
[[ ${1:-} == --slot ]] && { slot=${2:?--slot needs a number}; shift 2; }
hm_check_slot "$slot"
root=$(hm_root "$slot")
port=$(($(hm_base_port "$slot") + 5))
url=http://127.0.0.1:$port
log=$root/logs/booking.jsonl
name=booking-site

cmd=${1:-}
[[ -n $cmd ]] || usage
shift
case $cmd in
  up)
    today=()
    if [[ ${1:-} == --today ]]; then today=(--today "${2:?--today needs YYYY-MM-DD}"); shift 2; fi
    (($# == 0)) || usage
    if hm_svc_pid "$root" "$name" >/dev/null && curl -fsS -m 2 -o /dev/null "$url/_fake/health" 2>/dev/null; then
      ((${#today[@]} == 0)) || hm_die "already up: scripts/booking.sh down first to pin another date"
      echo "$url"
      exit 0
    fi
    [[ -d $root/home ]] || hm_die "slot $slot is not set up: scripts/test-env.sh up --slot $slot"
    [[ -z $(hm_listening "$port" "$port") ]] || HM_DIE_CODE=3 hm_die "port $port is in use"
    [[ -x $HM_REPO/bin/hommy-e2e ]] || "$HM_REPO/scripts/dev-build.sh" >/dev/null
    mkdir -p "$root/logs" "$root/run"
    touch "$log"
    "$HM_REPO/scripts/sx.sh" --slot "$slot" python3 "$HM_REPO/scripts/lib/detach.py" --pid-file "$root/run/$name.pid" \
      --log "$root/logs/$name.log" -- "$HM_REPO/bin/hommy-e2e" booking-site --port "$port" --log "$log" ${today[@]+"${today[@]}"}
    hm_wait_http "$url/_fake/health" || hm_die "the booking site did not answer on :$port; $(tail -3 "$root/logs/$name.log" 2>/dev/null)"
    echo "$url" ;;
  down) hm_svc_stop "$root" "$name"; echo down ;;
  log) [[ -f $log ]] && cat "$log" ;;
  reset)
    : >"$log"
    if hm_svc_pid "$root" "$name" >/dev/null; then curl -fsS -m 5 -X POST "$url/_fake/reset" >/dev/null; fi
    echo reset ;;
  bookings) curl -fsS -m 5 "$url/_fake/bookings" ;;
  url) echo "$url" ;;
  status)
    if hm_svc_pid "$root" "$name" >/dev/null && curl -fsS -m 2 -o /dev/null "$url/_fake/health" 2>/dev/null; then
      echo "up $url"
    else
      echo down
      exit 1
    fi ;;
  *) usage ;;
esac
