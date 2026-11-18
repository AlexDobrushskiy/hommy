#!/bin/bash
# The clean-room run (spec 06): a stranger's install in a fresh Tart VM with the real test bot, timed and logged.
#   scripts/clean-room.sh run [--until setup]   fresh VM hommy-06 from the cached image, artifacts in, the README's
#                                               unattended install (--non-interactive --token-stdin, the real test bot's
#                                               token on stdin), per-stage timings. --until setup: print the pairing
#                                               code and wait for the install to end (the tester pairs with
#                                               scripts/tg-real.sh). Without it: pair, doctor, place, the voice errand
#                                               and its checks, all here (tg=real: each send raises the owner's guard)
#   scripts/clean-room.sh place NAME            the booking fixture on the VM's 127.0.0.1:4715 + NAME's row in the
#                                               vault's context/places.md (scripts/vault.sh add-place): a non-user step
#   scripts/clean-room.sh rerun [FLAGS…]        the same install command again (+ FLAGS, e.g. --with-whatsapp)
#   scripts/clean-room.sh upgrade               artifacts with a bumped version, the install command again, and the
#                                               before/after record (config, vault, Keychain items, services)
#   scripts/clean-room.sh collect               the VM's logs into the run's logs/ (token audit: matches 0)
#   scripts/clean-room.sh down                  collect, delete the VM, host disk after
#   scripts/clean-room.sh status                the current run's dir and the VM's state
# Run dirs: .dev/clean-room/<run>/ (`current` points at the newest): run.env (date, image, disk), timings.tsv
# (stage<TAB>seconds), install-*.txt, tg.jsonl, doctor.txt, errand checks, logs/. Exit: 0 ok, 1 a step failed,
# 2 usage, 3 refused (vm.sh's disk/image guard; no test bot configured).
set -euo pipefail
HM_ME=clean-room
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
cd "$HM_REPO"

NAME=hommy-06
BASE=$HM_REPO/.dev/clean-room
VM=$HM_REPO/scripts/vm.sh
TGR=$HM_REPO/scripts/tg-real.sh
FIXTURE_PORT=4715
INSTALL_TIMEOUT=${HOMMY_CR_INSTALL_TIMEOUT:-5400} # s; the pairing wait is inside it
ERRAND_TIMEOUT=600
UPGRADE_VERSION=${HOMMY_CR_UPGRADE_VERSION:-0.1.1-dev}
# The README's unattended form (docs: README › Setup flags); --source points it at the copied artifacts.
INSTALL=(./install.sh --source artifacts --non-interactive --token-stdin)

usage() { sed -n '2,21s/^# \{0,1\}//p' "$0" >&2; exit 2; }
say() { printf '%s\n' "$*"; }
now() { date +%s; }
tart() { "$("$HM_REPO/scripts/tools.sh" path tart)" "$@"; }
vmr() { "$VM" run "$NAME" -- "$@"; }
vmr_in() { "$VM" run "$NAME" --stdin -- "$@"; }
rundir() {
  [[ -L $BASE/current ]] || hm_die "no clean-room run yet (scripts/clean-room.sh run)"
  (cd "$BASE/current" && pwd -P)
}
# The bot token never leaves a pipe: Keychain → ssh stdin → setup's --token-stdin.
bot_token() { security find-generic-password -s hommy-test-bot-token -w; }
nonuser() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>"$RD/nonuser.log"; }

# Runs the install command in the VM in the background: stamped output in $RD/install-$1.txt (epoch<TAB>line), its
# exit code in $RD/install-$1.rc. Extra args go to install.sh.
install_bg() {
  local tag=$1 out rc
  shift
  out=$RD/install-$tag.txt rc=$RD/install-$tag.rc
  rm -f "$out" "$rc"
  : >"$out"
  (
    set +e
    bot_token | timeout "$INSTALL_TIMEOUT" "$VM" run "$NAME" --stdin -- "${INSTALL[@]}" "$@" 2>&1 |
      perl -MTime::HiRes=time -ne 'BEGIN { $| = 1 } printf "%.1f\t%s", time, $_' >>"$out"
    echo "${PIPESTATUS[1]}" >"$rc"
  ) </dev/null &
  INSTALL_PID=$!
}
# Follows install-$1.txt: prints each `[n/9]` and `install:` line once; returns when the install ended (0) or, with
# --code, as soon as setup printed its pairing code (sets CODE; 4 = the install ended without one).
follow_install() {
  local tag=$1 want_code=${2:-} out shown=0 lines
  out=$RD/install-$tag.txt
  while :; do
    lines=$(wc -l <"$out" | tr -d ' ')
    if ((lines > shown)); then
      sed -n "$((shown + 1)),${lines}p" "$out" | cut -f2- | grep -E '^\[[0-9]/9\]|^install:|^setup: |^code: |^install: ' || true
      shown=$lines
    fi
    if [[ -n $want_code ]]; then
      CODE=$(cut -f2- "$out" | sed -n 's/^code: \([A-Z0-9-]*\).*/\1/p' | head -n 1)
      [[ -n $CODE ]] && return 0
    fi
    if [[ -f $RD/install-$tag.rc ]]; then
      wait "$INSTALL_PID" 2>/dev/null || true
      [[ -z $want_code ]] || return 4
      return 0
    fi
    sleep 2
  done
}

# timings.tsv from the stamped output: one line per stage with seconds, then total.
timings() {
  local out=$RD/install-first.txt
  python3 - "$RD" "$out" <<'PY'
import re, sys
rd, out = sys.argv[1], sys.argv[2]
env = dict(l.rstrip("\n").split("=", 1) for l in open(f"{rd}/run.env") if "=" in l)
rows = []
for k, name in (("t_artifacts", "artifacts"), ("t_create", "vm-create"), ("t_copy", "copy")):
    if k in env:
        rows.append((name, float(env[k])))
lines = []
for l in open(out, encoding="utf-8", errors="replace"):
    ts, _, text = l.rstrip("\n").partition("\t")
    try:
        lines.append((float(ts), text))
    except ValueError:
        pass
start = float(env.get("install_start", lines[0][0] if lines else 0))
prev, code_t, paired_t = start, None, None
for ts, text in lines:
    m = re.match(r"^\[(\d)/9\] ([^:]+):", text)
    if text.startswith("code: ") and code_t is None:
        code_t = ts
    if "✅ paired" in text and paired_t is None:
        paired_t = ts
    if m:
        n, stage = m.group(1), m.group(2).strip().lower().replace(" ", "-")
        d = ts - prev
        if n == "9" and code_t and paired_t:
            d -= paired_t - code_t
            rows.append((f"{n}-{stage}", d))
            rows.append(("pair-wait", paired_t - code_t))
        else:
            rows.append((f"{n}-{stage}", d))
        prev = ts
with open(f"{rd}/timings.tsv", "w") as f:
    for name, s in rows:
        f.write(f"{name}\t{s:.0f}\n")
total = sum(s for _, s in rows)
print(f"timings: {rd}/timings.tsv ({len(rows)} stages)")
print(f"total: {total:.0f} s ({total / 60:.1f} min; pairing wait included)")
PY
}

new_run() {
  local id img
  id=$(date +%Y%m%d-%H%M%S)
  RD=$BASE/$id
  mkdir -p "$RD"
  ln -sfn "$id" "$BASE/current"
  img=$(tart list --source oci --format json | python3 -c 'import json, sys
for v in json.load(sys.stdin):
    if "@sha256:" in v["Name"] and "golden-gate-vanilla" in v["Name"]:
        print(v["Name"].split("@", 1)[1]); break')
  {
    echo "run=$id"
    echo "date=$(date '+%Y-%m-%d %H:%M %Z')"
    echo "image=ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest@${img:-unknown}"
    echo "disk_before_gb=$(df -g "$HOME" | awk 'NR == 2 {print $4}')"
  } >"$RD/run.env"
  say "run: $id ($RD)"
}

cmd_run() {
  local until='' t0 rc
  while (($#)); do
    case $1 in
      --until) [[ ${2:-} == setup ]] || usage; until=setup; shift 2 ;;
      *) usage ;;
    esac
  done
  security find-generic-password -s hommy-test-bot-token >/dev/null 2>&1 ||
    HM_DIE_CODE=3 hm_die "no hommy-test-bot-token in the Keychain (the real test bot's token)"
  if [[ -z $until ]]; then
    [[ -n ${HOMMY_TEST_BOT:-} || -r ${XDG_CONFIG_HOME:-$HOME/.config}/hommy-test/test-bot ]] ||
      HM_DIE_CODE=3 hm_die "no test bot configured (HOMMY_TEST_BOT or ~/.config/hommy-test/test-bot)"
  fi
  [[ $("$VM" status "$NAME") == absent ]] || { say "deleting the previous $NAME"; "$VM" delete "$NAME" >/dev/null; }
  new_run

  t0=$(now)
  scripts/vm-artifacts.sh | tail -n 1
  echo "t_artifacts=$(($(now) - t0))" >>"$RD/run.env"
  echo "version=$(cat .dev/artifacts/VERSION)" >>"$RD/run.env"

  t0=$(now)
  "$VM" create "$NAME" || exit $?
  echo "t_create=$(($(now) - t0))" >>"$RD/run.env"
  cp "$HM_REPO/.dev/vm/$NAME/steps.log" "$RD/vm-steps-create.log" 2>/dev/null || true

  t0=$(now)
  "$VM" copy "$NAME" .dev/artifacts/ artifacts >/dev/null
  "$VM" copy "$NAME" .dev/artifacts/install.sh install.sh >/dev/null
  echo "t_copy=$(($(now) - t0))" >>"$RD/run.env"
  vmr /bin/zsh -c 'command -v claude || echo "claude: none (fresh)"' >"$RD/pre-claude.txt" 2>&1 || true

  echo "install_start=$(now)" >>"$RD/run.env"
  say "install: ${INSTALL[*]} (bot token on stdin)"
  install_bg first
  rc=0
  follow_install first --code || rc=$?
  if ((rc == 4)); then
    say "install ended before pairing: exit $(cat "$RD/install-first.rc") (output: $RD/install-first.txt)"
    timings
    exit 1
  fi
  if [[ -n $until ]]; then
    say "pairing: send the test bot  /start $CODE  (scripts/tg-real.sh send text \"/start $CODE\" --wait reply --timeout 60)"
    say "waiting for the install to end (setup waits at pairing)…"
  else
    say "pairing: /start $CODE"
    "$TGR" send text "/start $CODE" --wait reply --timeout 60 >"$RD/tg-pair.json" || true
    jq -r '.data.wait.message.text // .error.message // "no reply"' "$RD/tg-pair.json" | head -n 1
  fi
  follow_install first
  rc=$(cat "$RD/install-first.rc")
  say "install exit: $rc"
  echo "install_rc=$rc" >>"$RD/run.env"
  timings
  ((rc == 0)) || exit 1
  [[ -z $until ]] || exit 0

  # E2–E4 (the full flow, as the tester would type them)
  rc=0
  vmr hommy doctor >"$RD/doctor.txt" 2>&1 || rc=$?
  say "doctor: exit $rc ($RD/doctor.txt)"
  say "pair --status: $(vmr hommy pair --status 2>&1 | tail -n 1)"
  cmd_place "Barbearia Exemplo"
  errand
}

# The booking fixture inside the VM and the place row: the one non-user step besides the VM's own (README).
cmd_place() {
  local name=${1:?place NAME}
  RD=$(rundir)
  vmr_in /bin/zsh -s "$name" "$FIXTURE_PORT" <<'ZSH'
name=$1 port=$2
mkdir -p ~/.hommy-vm/booking && cd ~/.hommy-vm/booking
old=$(lsof -t -iTCP:$port -sTCP:LISTEN) && { kill $old; sleep 0.5; }
: > log.jsonl
nohup ~/artifacts/fakes/hommy-e2e booking-site --port $port --log $PWD/log.jsonl </dev/null >>site.log 2>&1 &!
for i in {1..50}; do curl -fsS -m 1 -o /dev/null http://127.0.0.1:$port/_fake/health 2>/dev/null && break; sleep 0.2; done
curl -fsS -m 2 -o /dev/null http://127.0.0.1:$port/_fake/health || { echo "booking site: not up"; exit 1; }
vault=$(hommy config get VAULT_DIR 2>/dev/null) || vault=$HOME/hommy-vault
~/artifacts/app/scripts/vault.sh add-place "$vault" "$name" "http://127.0.0.1:$port/" --kind barber
grep -F "| $name |" "$vault/context/places.md"
echo "fixture log: ~/.hommy-vm/booking/log.jsonl"
ZSH
  nonuser "place: booking fixture hommy-e2e booking-site on 127.0.0.1:$FIXTURE_PORT + vault row '$name' (scripts/vault.sh add-place)"
}

# Waits for bot replies until one carries the Book/Cancel buttons; every reply is appended to tg.jsonl.
wait_until() {
  local what=$1 deadline=$(($(now) + ERRAND_TIMEOUT)) left r
  while :; do
    left=$((deadline - $(now)))
    ((left > 0)) || { say "timeout waiting for $what"; return 1; }
    r=$("$TGR" wait --for reply --timeout "$left") || { say "no reply ($what)"; return 1; }
    jq -c '.data.wait.message' <<<"$r" >>"$RD/tg.jsonl"
    jq -r '.data.wait.message | "  bot #\(.id): \(.text | split("\n")[0] | .[0:120])\(if .media then " [\(.media.type)]" else "" end)\(if .inline_keyboard then " [buttons: \([.inline_keyboard[][].text] | join("/"))]" else "" end)"' <<<"$r"
    case $what in
      buttons) jq -e '.data.wait.message.inline_keyboard // [] | flatten | map(.text) | index("Book")' <<<"$r" >/dev/null && { LAST=$r; return 0; } ;;
      text) jq -e '.data.wait.message | (.media == null) and (.text | length > 0)' <<<"$r" >/dev/null && { LAST=$r; return 0; } ;;
      voice) jq -e '.data.wait.message.media.type == "voice"' <<<"$r" >/dev/null && { LAST=$r; return 0; } ;;
    esac
  done
}

errand() {
  local msg tomorrow posts
  RD=$(rundir)
  : >"$RD/tg.jsonl"
  tomorrow=$(vmr date -v+1d +%F | tr -d '\r')
  say "errand: voice note (book-haircut.ogg), tomorrow in the VM = $tomorrow"
  echo "errand_start=$(now)" >>"$RD/run.env"
  "$TGR" send voice e2e/fixtures/audio/book-haircut.ogg >"$RD/tg-voice.json"
  wait_until buttons || return 1
  msg=$(jq -r '.data.wait.message.id' <<<"$LAST")
  echo "t_errand_buttons=$(($(now) - $(sed -n 's/^errand_start=//p' "$RD/run.env")))" >>"$RD/run.env"
  say "press Book on #$msg"
  "$TGR" press --msg "$msg" --button Book --wait edit >"$RD/tg-press.json" || say "press: no edit"
  wait_until text || return 1
  wait_until voice || return 1
  echo "t_errand_total=$(($(now) - $(sed -n 's/^errand_start=//p' "$RD/run.env")))" >>"$RD/run.env"
  # shellcheck disable=SC2016 # expands in the VM
  vmr /bin/zsh -c 'cat ~/.local/share/hommy/briefs/*/brief.md' >"$RD/brief.md" 2>&1 || true
  say "brief.md (the 🆕 brief): $(head -c 200 "$RD/brief.md")"
  vmr cat .hommy-vm/booking/log.jsonl >"$RD/fixture-log.jsonl"
  posts=$(jq -c "select(.method == \"POST\" and .path == \"/bookings\")" "$RD/fixture-log.jsonl")
  say "fixture POST /bookings: $(grep -c . <<<"$posts" || true) — $(jq -r '.form | "\(.date) \(.time)"' <<<"$posts" 2>/dev/null | tr '\n' ' ')"
  # shellcheck disable=SC2016 # expands in the VM
  vmr /bin/zsh -c 'tail -n 3 "$(hommy config get VAULT_DIR)/log.md"' >"$RD/vault-log-tail.txt" 2>&1 || true
  say "vault log.md tail:"; sed 's/^/  /' "$RD/vault-log-tail.txt"
  sleep 20 # the lane's close is asynchronous to the answer
  vmr .local/share/hommy-browsers/browser-reaper --json >"$RD/reaper.json" 2>&1 || true
  say "browser-reaper: $(head -c 300 "$RD/reaper.json")"
}

# Labels, PIDs and run counts of the instance's launchd jobs (for rerun/upgrade).
services() {
  vmr_in /bin/zsh -s <<'ZSH'
for l in $(launchctl list | awk '$3 ~ /^dev\.hommy\./ {print $3}' | sort); do
  launchctl print gui/$UID/$l 2>/dev/null |
    awk -v l=$l -F' = ' '$1 ~ /^\tpid$/ {p=$2} $1 ~ /^\truns$/ {r=$2} END {print l " pid=" (p ? p : "-") " runs=" (r ? r : "-")}'
done
ZSH
}
# Hashes of what an upgrade must keep: config, vault, Keychain item attributes (never their secrets).
keep_record() {
  vmr_in /bin/zsh -s <<'ZSH'
c=~/.config/hommy v=$(hommy config get VAULT_DIR)
for f in $c/hommy.env $c/llama-swap.yaml ~/.local/share/hommy-browsers/config.env; do
  print -r -- "$(shasum -a 256 <$f | cut -c1-16) ${f/#$HOME/~}"
done
print -r -- "$(cd $v && find . -type f ! -path './.git/*' -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256 | cut -c1-16) vault ($(cd $v && find . -type f ! -path './.git/*' | wc -l | tr -d ' ') files)"
# attributes only (dump-keychain without -d prints no secret), the hommy items, incl. their modification dates
items=$(security dump-keychain ~/Library/Keychains/login.keychain-db 2>/dev/null |
  awk '/^keychain:/ { if (r ~ /"svce"<blob>="hommy/) print r; r = "" } { r = r $0 "\n" } END { if (r ~ /"svce"<blob>="hommy/) print r }')
print -r -- "$(print -r -- $items | shasum -a 256 | cut -c1-16) keychain items ($(print -r -- $items | grep -c '"svce"<blob>="hommy'))"
ZSH
}

cmd_rerun() {
  local n
  RD=$(rundir)
  n=$(find "$RD" -maxdepth 1 -name 'install-rerun*.txt' | wc -l | tr -d ' ')
  n=rerun$((n + 1))
  services >"$RD/$n-services-before.txt"
  say "$n: ${INSTALL[*]} $*"
  install_bg "$n" "$@"
  follow_install "$n"
  services >"$RD/$n-services-after.txt"
  say "install exit: $(cat "$RD/install-$n.rc")"
  say "services (label pid runs), before → after:"
  if diff "$RD/$n-services-before.txt" "$RD/$n-services-after.txt" >/dev/null; then
    say "  unchanged ($(wc -l <"$RD/$n-services-after.txt" | tr -d ' ') jobs)"
  else
    diff "$RD/$n-services-before.txt" "$RD/$n-services-after.txt" | sed 's/^/  /' || true
  fi
  (($(cat "$RD/install-$n.rc") == 0))
}

cmd_upgrade() {
  local up
  RD=$(rundir)
  up=$RD/artifacts-up
  scripts/vm-artifacts.sh --version "$UPGRADE_VERSION" --out "$up" | tail -n 1
  services >"$RD/upgrade-services-before.txt"
  keep_record >"$RD/upgrade-keep-before.txt"
  say "version before: $(vmr hommy version 2>&1 | head -n 1)"
  "$VM" copy "$NAME" "$up/" artifacts >/dev/null
  "$VM" copy "$NAME" "$up/install.sh" install.sh >/dev/null
  say "upgrade: ${INSTALL[*]}"
  install_bg upgrade
  follow_install upgrade
  services >"$RD/upgrade-services-after.txt"
  keep_record >"$RD/upgrade-keep-after.txt"
  say "install exit: $(cat "$RD/install-upgrade.rc")"
  say "version after: $(vmr hommy version 2>&1 | head -n 1)"
  say "kept (config, vault, keychain):"
  if diff "$RD/upgrade-keep-before.txt" "$RD/upgrade-keep-after.txt" >/dev/null; then
    sed 's/^/  = /' "$RD/upgrade-keep-after.txt"
  else
    diff "$RD/upgrade-keep-before.txt" "$RD/upgrade-keep-after.txt" | sed 's/^/  /' || true
  fi
  say "services (label pid runs), before → after:"
  paste -d'|' "$RD/upgrade-services-before.txt" "$RD/upgrade-services-after.txt" | sed 's/|/  →  /; s/^/  /'
  say "host name: $(vmr hommy config get HOST_NAME 2>&1 | tail -n 1)"
  (($(cat "$RD/install-upgrade.rc") == 0))
}

cmd_collect() {
  local tok hits
  RD=$(rundir)
  [[ $("$VM" status "$NAME") == running* ]] || { say "collect: $NAME is not running"; return 0; }
  mkdir -p "$RD/logs"
  vmr_in /bin/zsh -s >/dev/null 2>&1 <<'ZSH' || true
rm -rf ~/.hommy-vm/collect && mkdir -p ~/.hommy-vm/collect && cd ~/.hommy-vm/collect || exit 1
rsync -a --max-size=20m --exclude '*.bin' --exclude '*.gguf' ~/.local/state/ state/
rsync -a ~/.hommy-vm/booking ~/.hommy-vm/faketg . 2>/dev/null
rsync -a ~/.local/share/hommy/briefs . 2>/dev/null
v=$(hommy config get VAULT_DIR 2>/dev/null) && cp "$v/log.md" vault-log.md
hommy doctor >doctor.txt 2>&1
launchctl list | grep dev.hommy >launchd.txt
true
ZSH
  local ip
  ip=$("$VM" ip "$NAME")
  rsync -a -e "ssh -F /dev/null -i $HM_REPO/.dev/vm/$NAME/id_ed25519 -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile=$HM_REPO/.dev/vm/$NAME/known_hosts -o LogLevel=ERROR" \
    "admin@$ip:.hommy-vm/collect/" "$RD/logs/" || true
  cp "$HM_REPO/.dev/vm/$NAME/steps.log" "$RD/vm-steps.log" 2>/dev/null || true
  # No secret in what we keep: count the two tokens' occurrences, never print them.
  hits=0
  for tok in hommy-test-bot-token hommy-test-claude-token; do
    local v
    v=$(security find-generic-password -s "$tok" -w 2>/dev/null) || continue
    [[ -n $v ]] || continue
    # the value goes to grep on a file descriptor (printf is a builtin): never in an argv `ps` shows
    hits=$((hits + $(grep -rlF -f <(printf '%s\n' "$v") "$RD" 2>/dev/null | wc -l | tr -d ' ')))
  done
  say "collected: $RD/logs ($(find "$RD/logs" -type f | wc -l | tr -d ' ') files); token matches in $RD: $hits"
  ((hits == 0)) || hm_die "a token is in the collected files: remove them ($RD)"
}

cmd_down() {
  RD=$(rundir)
  cmd_collect || true
  "$VM" delete "$NAME"
  echo "disk_after_gb=$(df -g "$HOME" | awk 'NR == 2 {print $4}')" >>"$RD/run.env"
  say "disk: $(sed -n 's/^disk_before_gb=//p' "$RD/run.env") GB free before, $(df -g "$HOME" | awk 'NR == 2 {print $4}') GB after"
}

cmd_status() {
  if [[ -L $BASE/current ]]; then say "run: $(rundir)"; else say "run: none"; fi
  say "vm: $NAME $("$VM" status "$NAME")"
}

(($#)) || usage
cmd=$1
shift
case $cmd in
  run) cmd_run "$@" ;;
  place) (($# == 1)) || usage; cmd_place "$1" ;;
  rerun) cmd_rerun "$@" ;;
  upgrade) (($# == 0)) || usage; cmd_upgrade ;;
  collect) cmd_collect ;;
  down) cmd_down ;;
  status) cmd_status ;;
  -h | --help) usage ;;
  *) usage ;;
esac
