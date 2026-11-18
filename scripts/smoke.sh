#!/bin/bash
# The phase's critical path, fast: build, `hommy version`, `hommy` with no args (usage, exit 2), the slot's test env
# (brought up when down), the test instance's config (`hommy config path` under sx, 0600, written by `up`), a fake
# Telegram round trip, a voice upload and download, a fake transcription, the claude stub through sx, the leak gate's
# self-test (when present) and the live guard, all under the keychain guard (no keychain dialog, the real keychain
# search list untouched). Prints PASS/FAIL per check; exit 1 on any FAIL.
set -euo pipefail
HM_ME=smoke
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
cd "$HM_REPO"
slot=$(hm_default_slot)
export HOMMY_SLOT=$slot
sx=(scripts/sx.sh --slot "$slot")
pass=0 fail=0 tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

check() { # NAME FUNCTION
  local out
  if out=$("$2" 2>&1); then
    pass=$((pass + 1))
    printf 'PASS %s\n' "$1"
  else
    fail=$((fail + 1))
    printf 'FAIL %s: %s\n' "$1" "$(tail -3 <<<"$out" | tr '\n' ' ')"
  fi
}

c_build() { scripts/dev-build.sh; }
c_secret() {
  [[ $("${sx[@]}" security find-generic-password -s "$(hm_service "$slot")" -a telegram-bot-token -w) == "$(cat "$(hm_root "$slot")/run/faketg.token")" ]]
}
c_kcguard() { scripts/keychain-guard.sh check; }
c_version() { "${sx[@]}" bin/hommy version | grep -q '^hommy '; }
c_usage() {
  local rc=0 err
  err=$(bin/hommy 2>&1 >/dev/null) || rc=$?
  [[ $rc == 2 ]] || { echo "exit $rc, want 2"; return 1; }
  grep -qi usage <<<"$err"
}
c_config() {
  local want f
  want=$(hm_root "$slot")/home/.config/hommy-$(hm_instance "$slot")/hommy.env
  f=$(scripts/hm.sh --slot "$slot" config path)
  [[ $f == "$want" ]] || { echo "config path: $f, want $want"; return 1; }
  [[ $(stat -f %Lp "$f") == 600 ]] || { echo "mode $(stat -f %Lp "$f")"; return 1; }
  grep -q "^PORT_BASE=$(hm_base_port "$slot")$" "$f"
}
c_env() { scripts/test-env.sh up >/dev/null; scripts/test-env.sh status >/dev/null; }

c_telegram() {
  local mark bot u id
  mark="smoke-$(openssl rand -hex 4)"
  bot=$(scripts/tg.sh bot-url)
  scripts/tg.sh send text "$mark" --from owner >/dev/null
  u=$("${sx[@]}" curl -fsS "$bot/getUpdates")
  id=$(jq -r --arg m "$mark" '.result[] | select(.message.text == $m) | .update_id' <<<"$u")
  [[ -n $id ]] || { echo "the update with $mark never arrived"; return 1; }
  jq -e --arg m "$mark" '.result[] | select(.message.text == $m) | .message.from.id == 1001 and .message.chat.type == "private"' <<<"$u" >/dev/null
  "${sx[@]}" curl -fsS "$bot/getUpdates" -d offset=$((id + 1)) -d timeout=0 >/dev/null
  "${sx[@]}" curl -fsS "$bot/sendMessage" -d chat_id=1001 -d "text=pong $mark" >/dev/null
  scripts/tg.sh read messages --limit 1 | jq -e --arg m "pong $mark" '.data.messages[-1] | .text == $m and .from == "bot"' >/dev/null
}

c_voice() {
  local bot u fid path
  bot=$(scripts/tg.sh bot-url)
  scripts/tg.sh send voice e2e/fixtures/audio/hello.ogg >/dev/null
  u=$("${sx[@]}" curl -fsS "$bot/getUpdates")
  fid=$(jq -r '[.result[] | select(.message.voice)] | last | .message.voice.file_id' <<<"$u")
  path=$("${sx[@]}" curl -fsS "$bot/getFile" -d "file_id=$fid" | jq -r .result.file_path)
  "${sx[@]}" curl -fsS "$(scripts/tg.sh url)/file/bot$(scripts/tg.sh token)/$path" -o "$tmp/voice.ogg"
  "${sx[@]}" curl -fsS "$bot/getUpdates" -d "offset=$(($(jq '[.result[].update_id] | max' <<<"$u") + 1))" -d timeout=0 >/dev/null
  [[ $(shasum -a 256 <"$tmp/voice.ogg") == "$(shasum -a 256 <e2e/fixtures/audio/hello.ogg)" ]]
}

c_speech() {
  "${sx[@]}" curl -fsS -F file=@e2e/fixtures/audio/book-haircut.ogg -F model=whisper "$(scripts/speech.sh url)/v1/audio/transcriptions" |
    jq -e '.text == "book a haircut at Barbearia Exemplo tomorrow at ten thirty"' >/dev/null
}

c_stub() {
  printf 'echo: smoke stub' | "${sx[@]}" claude-stub -p --output-format stream-json --verbose --include-partial-messages \
    --dangerously-skip-permissions | jq -se 'last | .type == "result" and .result == "smoke stub"' >/dev/null
}

c_leak() { scripts/leak-scan.sh --self-test | tail -1 | grep -q 'self-test: PASS'; }
c_guard() { scripts/live-guard.sh check; }

scripts/keychain-guard.sh start >/dev/null
check build c_build
check "hommy version" c_version
check "hommy without args: usage, exit 2" c_usage
check "test env up" c_env
check "test instance config (path, 0600, written by up)" c_config
check "scratch keychain secret read" c_secret
check "telegram round trip" c_telegram
check "voice upload and download" c_voice
check "fake transcription" c_speech
check "claude stub (echo:)" c_stub
if [[ -x scripts/leak-scan.sh ]]; then check "leak-scan --self-test" c_leak; else echo "SKIP leak-scan --self-test (no scripts/leak-scan.sh yet)"; fi
check "live guard" c_guard
check "keychain guard (no dialog, real keychain list unchanged)" c_kcguard
if ((fail)); then
  echo "smoke: FAIL ($fail of $((pass + fail)))"
  exit 1
fi
echo "smoke: PASS ($pass checks)"
