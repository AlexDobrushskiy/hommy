#!/bin/bash
# tg=real rows only: scripts/tg.sh's shared verbs (send text|voice, react, press, stop, read, wait; same JSON and exit
# codes) against the real **test bot**, driven from the maintainer's account with telegram-agent-cli e2e
# (docs/testing.md §4, §12). Every call reaches real Telegram, so the guard hook asks for the owner's OK.
# Safety, checked before the CLI is ever invoked (exit 2, a usage JSON):
#   - the test bot comes only from HOMMY_TEST_BOT or ${XDG_CONFIG_HOME:-~/.config}/hommy-test/test-bot (one line,
#     `@hommy_test_<6 chars>_bot`, the name testing.md §12 creates); unset or another shape: refused. It is always
#     passed as --chat, so the CLI's default chat is never used;
#   - fake-only verbs (token, bot-url, url, calls, initdata, fault, reset) and flags (--chat; --from owner|other, and
#     --from on anything but `read`, whose --from bot|me|any is the CLI's own filter) are refused.
# Like the fake, `wait --for reply` starts after the newest message this tester has seen (a send, a `read messages`,
# the message a reply wait returned), so a reply that lands before the wait starts is not missed: the id is kept in
# /tmp/hm-s<N>/run/tgreal.seen and passed as --since unless the call gives its own.
set -euo pipefail
HM_ME=tg-real
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
usage_json() {
  printf '{"schema_version":"1","ok":false,"error":{"code":"usage","message":"%s"}}\n' "$1"
  exit 2
}
(($#)) || usage_json "a verb is required: send|react|press|stop|read|wait"
verb=$1
case $verb in
  send | react | press | stop | read | wait) ;;
  token | bot-url | url | calls | initdata | fault | reset) usage_json "$verb is fake-only (scripts/tg.sh)" ;;
  *) usage_json "unknown verb $verb" ;;
esac
prev='' since='' reply=''
for a in "$@"; do
  case $a in
    --since | --since=*) since=1 ;;
    --for=reply) reply=1 ;;
    reply) [[ $prev != --for ]] || reply=1 ;;
    --chat | --chat=*) usage_json "--chat is fake-only; the test bot comes from the test config" ;;
    --from=owner | --from=other) usage_json "--from owner|other is fake-only" ;;
    --from=*) [[ $verb == read ]] || usage_json "--from is fake-only outside read" ;;
    --from) [[ $verb == read ]] || usage_json "--from is fake-only outside read" ;;
    owner | other) [[ $prev != --from ]] || usage_json "--from owner|other is fake-only" ;;
  esac
  prev=$a
done
bot=${HOMMY_TEST_BOT:-}
cfg=${XDG_CONFIG_HOME:-$HOME/.config}/hommy-test/test-bot
if [[ -z $bot && -r $cfg ]]; then bot=$(head -n 1 "$cfg" | tr -d '[:space:]'); fi
[[ -n $bot ]] || usage_json "no test bot configured (HOMMY_TEST_BOT or $cfg; docs/testing.md §12)"
[[ $bot == @* ]] || bot=@$bot
[[ $bot =~ ^@hommy_test_[a-z0-9]{6}_bot$ ]] || usage_json "test bot must be @hommy_test_<6 chars>_bot; refusing"
# `send voice|photo|file FILE` (scripts/tg.sh's form): the CLI takes the file as --path.
if [[ $verb == send && ${2:-} =~ ^(voice|photo|file)$ && -n ${3:-} && ${3:-} != -* ]]; then
  set -- "$1" "$2" --path "$3" "${@:4}"
fi
slot=$(hm_default_slot)
hm_check_slot "$slot"
seen_file=$(hm_root "$slot")/run/tgreal.seen
seen=0
if [[ -r $seen_file ]]; then
  read -r seen_bot seen_id <"$seen_file" || true
  [[ ${seen_bot:-} == "$bot" && ${seen_id:-} =~ ^[0-9]+$ ]] && seen=$seen_id
fi
if [[ $verb == wait && -n $reply && -z $since ]] && ((seen > 0)); then set -- "$@" --since "$seen"; fi

# The maintainer's account has one lock that every telegram-agent-cli on this Mac shares: a long wait would hold it
# and starve other users (lock_busy). A reply wait therefore runs in slices of at most SLICE s, the lock released
# between them, until one answers or the call's --timeout is used up (a timeout is exit 9, as one long wait gives).
# Each slice starts after the same id (--since; with none given, the newest message before the first slice), so a
# reply that lands between slices is still found. A slice refused with lock_busy (exit 6) is retried while time is
# left. The other kinds (edit, reaction, pin, keyboard, draft) compare with the state at the wait's own start, which a
# new slice would take again (missing a change made in the gap): they run as one wait.
SLICE=${HOMMY_TG_SLICE:-30}
# sliced_wait ARGS…: `e2e wait ARGS… --chat BOT` as above (no numeric --timeout: one call, the CLI's default)
sliced_wait() {
  local total='' rest=() kind='' anchored=0 t c o s0 left a
  while (($#)); do
    case $1 in
      --timeout) total=${2:-}; shift 2 ;;
      --timeout=*) total=${1#*=}; shift ;;
      --for) kind=${2:-}; rest+=("$1" "${2:-}"); shift 2 || shift ;;
      --for=*) kind=${1#*=}; rest+=("$1"); shift ;;
      --since | --since=*) anchored=1; rest+=("$1"); shift ;;
      *) rest+=("$1"); shift ;;
    esac
  done
  if ! [[ $total =~ ^[0-9]+$ ]] || [[ $kind != reply ]] || ((total <= SLICE)); then
    telegram-agent-cli e2e wait ${rest[@]+"${rest[@]}"} ${total:+--timeout "$total"} --chat "$bot"
    return
  fi
  if ((!anchored)); then
    a=$(telegram-agent-cli e2e read messages --limit 1 --chat "$bot" | jq -r '[.data.messages[]?.id? | numbers] | max // empty') || a=''
    if ! [[ $a =~ ^[0-9]+$ ]]; then # no anchor: one wait, which anchors itself
      telegram-agent-cli e2e wait ${rest[@]+"${rest[@]}"} --timeout "$total" --chat "$bot"
      return
    fi
  fi
  left=$total
  while :; do
    t=$((left < SLICE ? left : SLICE))
    ((t > 0)) || t=1
    c=0 s0=$SECONDS
    # the CLI's own wait for the lock counts against the time left
    o=$(TGAC_LOCK_WAIT=$left telegram-agent-cli e2e wait ${rest[@]+"${rest[@]}"} --timeout "$t" ${a:+--since "$a"} \
      --chat "$bot") || c=$?
    case $c in
      9) left=$((left - (SECONDS - s0 > t ? SECONDS - s0 : t))) ;;
      6) left=$((left - (SECONDS - s0 > 1 ? SECONDS - s0 : 1))); ((left > 0)) && sleep 1 && left=$((left - 1)) ;;
      *) left=0 ;;
    esac
    if ((left <= 0)); then
      printf '%s\n' "$o"
      return "$c"
    fi
  done
}

code=0
if [[ $verb == wait ]]; then
  shift
  out=$(sliced_wait "$@") || code=$?
elif [[ $verb == send ]] && [[ " $* " == *" --wait reply "* || " $* " == *" --wait=reply "* ]]; then
  # `send … --wait reply`: the send, then a sliced reply wait after the sent message (the same JSON: sent + wait)
  sargs=() wargs=(--for reply)
  shift
  while (($#)); do
    case $1 in
      --wait) shift 2 ;;
      --wait=*) shift ;;
      --timeout) wargs+=(--timeout "${2:-}"); shift 2 ;;
      --timeout=*) wargs+=("$1"); shift ;;
      *) sargs+=("$1"); shift ;;
    esac
  done
  sent=$(telegram-agent-cli e2e send ${sargs[@]+"${sargs[@]}"} --chat "$bot") || code=$?
  out=$sent
  sid=$(jq -r '.data.sent.id // empty' <<<"$sent" 2>/dev/null) || sid=''
  if ((code == 0)) && [[ $sid =~ ^[0-9]+$ ]]; then
    w=$(sliced_wait "${wargs[@]}" --since "$sid") || code=$?
    wj=$(jq -c '.data.wait // null' <<<"$w" 2>/dev/null) || wj=null
    ej=$(jq -c '.error // null' <<<"$w" 2>/dev/null) || ej=null
    out=$(jq -c --argjson w "${wj:-null}" --argjson e "${ej:-null}" \
      '.data.wait = $w | if $e != null then .ok = false | .error = $e else . end' <<<"$sent")
  fi
else
  out=$(telegram-agent-cli e2e "$@" --chat "$bot") || code=$?
fi
printf '%s\n' "$out"
# The newest id the tester saw: what it sent, read, or got from a reply wait (the fake's rule).
if ((code == 0)); then
  # a reply wait counts only the message it returned: the CLI also lists every reply that landed meanwhile in
  # .messages, and those are the next waits' answers
  id=$(jq -r '[.data.sent.id?, .data.messages[]?.id?,
      (.data.wait | select(.for? == "reply") | .message.id? // ([.messages[]?.id?] | min))] | map(numbers) | max // 0' \
    <<<"$out" 2>/dev/null) || id=0
  if [[ $id =~ ^[0-9]+$ ]] && ((id > seen)); then
    mkdir -p "${seen_file%/*}"
    printf '%s %s\n' "$bot" "$id" >"$seen_file.tmp" && mv -f "$seen_file.tmp" "$seen_file"
  fi
fi
exit "$code"
