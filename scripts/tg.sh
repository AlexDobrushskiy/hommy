#!/bin/bash
# Drives the fake Telegram's user side (docs/testing.md §4): the e2e harness verbs, JSON envelope and exit codes.
#   scripts/tg.sh send text [TEXT] [--message T] [--reply-to ID] [--wait reply|reaction|draft|…] [--timeout S] [--all]
#   scripts/tg.sh send voice [FILE.ogg | --path FILE.ogg] [--reply-to ID] [--wait …]
#   scripts/tg.sh react --msg ID (--emoji E | --remove) [--wait reaction]
#   scripts/tg.sh press (--msg ID | --keyboard) --button B [--wait edit|reaction]
#   scripts/tg.sh stop --draft ID
#   scripts/tg.sh read messages [--since N] [--limit 20] [--from bot|me|any] | read keyboard | read pinned
#   scripts/tg.sh wait --for reply|edit|reaction|pin|keyboard|draft [--since N] [--msg N] [--all] [--timeout 30]
# Fake-only extras: --from owner|other (send/react/press/stop) or --chat owner|other (read/wait),
#   token, bot-url, url, calls [--method M] [--since SEQ], initdata --user owner|other,
#   fault METHOD|file|* [--status 5xx] [--times N] | fault none, reset.
# tg=real rows use scripts/tg-real.sh (TG=real here exits 2). The fake is the slot's B+1 (HOMMY_SLOT, default from the
# checkout), or TG_URL.
set -euo pipefail
HM_ME=tg
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage_json() {
  printf '{"schema_version":"1","ok":false,"error":{"code":"usage","message":"%s"}}\n' "$1"
  exit 2
}

(($#)) || { sed -n '2,15s/^# \{0,1\}//p' "$0" >&2; exit 2; }
verb=$1

if [[ ${TG:-fake} == real ]]; then
  usage_json "scripts/tg.sh drives the fake only: run scripts/tg-real.sh for the real test bot"
fi

slot=$(hm_default_slot)
hm_check_slot "$slot"
url=${TG_URL:-http://127.0.0.1:$(($(hm_base_port "$slot") + 1))}

get() { # PATH: plain-text control endpoints
  curl -fsS -m 10 "$url/_fake/$1" || hm_die "fake Telegram not reachable at $url (scripts/test-env.sh up)"
}

case $verb in
  url) echo "$url"; exit 0 ;;
  token) get token; exit 0 ;;
  bot-url) get bot-url; exit 0 ;;
  reset) get reset; exit 0 ;;
  initdata)
    [[ ${2:-} == --user && -n ${3:-} ]] || usage_json "initdata --user owner|other"
    get "initdata?user=$3"
    exit 0 ;;
esac

# The voice file for `send voice`: --path F or the first positional argument after `voice`.
file=''
if [[ $verb == send && ${2:-} == voice ]]; then
  args=("${@:3}")
  for ((i = 0; i < ${#args[@]}; i++)); do
    case ${args[i]} in
      --path) file=${args[i + 1]:-}; break ;;
      --path=*) file=${args[i]#--path=}; break ;;
      --reply-to | --from | --chat | --wait | --timeout | --interval | --duration | --message) i=$((i + 1)) ;;
      --*) ;;
      *) file=${args[i]}; break ;;
    esac
  done
  [[ -n $file ]] || usage_json "send voice needs a file: send voice FILE.ogg"
  [[ -r $file ]] || usage_json "--path is not a readable file: $file"
fi

argv=$(jq -cn '$ARGS.positional' --args -- "$@")
form=(--form-string "args=$argv")
[[ -n $file ]] && form+=(-F "file=@$file;type=audio/ogg")
if ! out=$(curl -sS -m 600 -w '\n%header{x-exit-code}' "${form[@]}" "$url/_fake/cli" 2>/dev/null); then
  printf '{"schema_version":"1","ok":false,"error":{"code":"internal","message":"fake Telegram not reachable at %s (scripts/test-env.sh up)"}}\n' "$url"
  exit 1
fi
code=${out##*$'\n'}
body=${out%$'\n'*}
printf '%s\n' "${body%$'\n'}"
exit "${code:-1}"
