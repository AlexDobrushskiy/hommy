#!/bin/bash
# The slot's fake speech server (docs/testing.md §5), on B+2.
#   scripts/speech.sh url                    the base URL (…/v1/audio/transcriptions, …/v1/audio/speech)
#   scripts/speech.sh set-transcript TEXT    the next transcription returns TEXT (queued, first in first out)
#   scripts/speech.sh log                    one JSON line per request (incl. voice and lang_code)
#   scripts/speech.sh fault stt|tts|none     make STT or TTS answer 500, or clear it
#   scripts/speech.sh stop | start           stop or restart the server (outage rows)
set -euo pipefail
HM_ME=speech
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,7s/^# \{0,1\}//p' "$0" >&2; exit 2; }
slot=$(hm_default_slot)
hm_check_slot "$slot"
root=$(hm_root "$slot")
port=$(($(hm_base_port "$slot") + 2))
url=http://127.0.0.1:$port

post() { curl -fsS -m 10 -H 'content-type: application/json' -d "$2" "$url/_fake/$1" >/dev/null || hm_die "fake speech not reachable at $url"; }

case ${1:-} in
  url) echo "$url" ;;
  set-transcript)
    [[ -n ${2:-} ]] || usage
    post transcript "$(jq -cn --arg t "$2" '{text: $t}')"
    echo "queued" ;;
  log) curl -fsS -m 10 "$url/_fake/log" | jq -c '.requests[]' || hm_die "fake speech not reachable at $url" ;;
  fault)
    [[ ${2:-} =~ ^(stt|tts|none)$ ]] || usage
    post fault "{\"kind\":\"$2\"}"
    echo "fault: $2" ;;
  stop) hm_svc_stop "$root" fakespeech; echo stopped ;;
  start)
    hm_svc_pid "$root" fakespeech >/dev/null && { echo running; exit 0; }
    [[ -d $root/run ]] || hm_die "slot $slot is not set up: scripts/test-env.sh up"
    "$HM_REPO/scripts/sx.sh" --slot "$slot" python3 "$HM_REPO/scripts/lib/detach.py" --pid-file "$root/run/fakespeech.pid" \
      --log "$root/logs/fakespeech.log" -- "$HM_REPO/bin/hommy-e2e" fakespeech --addr "127.0.0.1:$port" \
      --fixtures "$HM_REPO/e2e/fixtures/audio"
    hm_wait_http "$url/health" || hm_die "fake speech did not start (see $root/logs/fakespeech.log)"
    echo started ;;
  *) usage ;;
esac
