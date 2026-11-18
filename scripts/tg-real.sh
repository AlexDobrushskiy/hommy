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
set -euo pipefail
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
prev=
for a in "$@"; do
  case $a in
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
exec telegram-agent-cli e2e "$@" --chat "$bot"
