#!/bin/bash
# The slot's test environment (docs/testing.md §2–3): scratch root /tmp/hm-s<N>/ (HOME, bin links, keychain, logs),
# test secrets in the scratch keychain, and the fakes started detached on the slot's ports.
#   scripts/test-env.sh up     [--slot N] [--layout browsers] [--no-start]   prints the ports, then `ready`
#   scripts/test-env.sh down   [--slot N] [--purge]                          stops everything; --purge removes the root
#   scripts/test-env.sh status [--slot N]                                    `up`, `partial` or `down` (exit 0 only for up)
#   scripts/test-env.sh home   [--slot N]                                    prints the scratch HOME
# Exit 3: a port of the slot is in use (named). The 02+ test instance's config is written by its own phase.
set -euo pipefail
HM_ME=test-env
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,9s/^# \{0,1\}//p' "$0" >&2; exit 2; }

cmd=${1:-}
[[ -n $cmd ]] || usage
shift
slot=$(hm_default_slot) layout=a start=1 purge=0
while (($#)); do
  case $1 in
    --slot) slot=${2:?--slot needs a number}; shift 2 ;;
    --layout) [[ ${2:-} == browsers ]] || hm_die "--layout takes only 'browsers'"; layout=b; shift 2 ;;
    --no-start) start=0; shift ;;
    --purge) purge=1; shift ;;
    *) usage ;;
  esac
done
hm_check_slot "$slot"
root=$(hm_root "$slot")
home=$root/home
B=$(hm_base_port "$slot")
kc=$(hm_keychain "$root")
svc=$(hm_service "$slot")

# Services by layout (testing.md §2): name port.
services() {
  local l=$layout
  [[ -f $root/run/layout ]] && l=$(cat "$root/run/layout")
  if [[ $l == b ]]; then echo "static-site $((B + 6))"; else printf 'faketg %s\nfakespeech %s\n' $((B + 1)) $((B + 2)); fi
}

svc_url() {
  case $1 in
    static-site) echo "http://127.0.0.1:$2/" ;;
    fakespeech) echo "http://127.0.0.1:$2/health" ;;
    *) echo "http://127.0.0.1:$2/_fake/health" ;;
  esac
}

# all_up: every service of the layout runs and answers.
all_up() {
  local name port n=0
  while read -r name port; do
    hm_svc_pid "$root" "$name" >/dev/null || return 1
    curl -fsS -m 2 -o /dev/null "$(svc_url "$name" "$port")" 2>/dev/null || return 1
    n=$((n + 1))
  done < <(services)
  ((n > 0))
}

any_running() {
  local name port
  while read -r name port; do hm_svc_pid "$root" "$name" >/dev/null && return 0; done < <(services)
  return 1
}

labels() { launchctl list 2>/dev/null | awk -v p="$(hm_label_prefix "$slot")" 'index($3, p) == 1 { print $3 }'; }

stop_all() {
  local name label f
  for name in faketg fakespeech static-site; do hm_svc_stop "$root" "$name"; done
  # and every other detached service of the slot (booking-site, …): one pid file each in run/
  for f in "$root"/run/*.pid; do [[ -f $f ]] && hm_svc_stop "$root" "$(basename "$f" .pid)"; done
  while read -r label; do
    [[ -n $label ]] && { launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true; }
  done < <(labels)
}

print_ports() {
  local name port
  printf 'slot %s · scratch root %s/\n' "$slot" "$root"
  printf '  %-12s %s\n' HOME "$home"
  while read -r name port; do printf '  %-12s http://127.0.0.1:%s\n' "$name" "$port"; done < <(services)
  printf '  %-12s %s-%s (backend %s-%s)\n' ports "$B" $((B + 9)) $((B + 10000)) $((B + 10009))
}

ensure_keychain() {
  mkdir -p "$home/Library/Keychains" "$home/Library/Preferences"
  if [[ ! -f $kc || ! -s $root/kc.pw ]]; then
    remove_keychain
    (umask 077 && openssl rand -hex 24 >"$root/kc.pw")
    hm_scratch_security "$home" create-keychain -p "$(cat "$root/kc.pw")" "$kc"
  fi
  hm_unlock_keychain "$root" || hm_die "scratch keychain not usable"
  hm_scratch_security "$home" list-keychains -d user -s "$kc"
  hm_scratch_security "$home" default-keychain -d user -s "$kc"
}

# remove_keychain: out of the scratch search list first, then deleted (unlocked, so delete never prompts).
remove_keychain() {
  [[ -f $kc ]] || return 0
  hm_scratch_security "$home" list-keychains -d user -s || true
  # delete-keychain only on an unlocked keychain; otherwise the file just goes (no Security call can prompt)
  if [[ -s $root/kc.pw ]] && hm_unlock_keychain "$root" 2>/dev/null; then
    hm_scratch_security "$home" delete-keychain "$kc" 2>/dev/null || true
  fi
  rm -f "$kc"
}

# put_secret SERVICE ACCOUNT: the value comes on stdin and reaches `security -i` over stdin only.
put_secret() {
  awk -v s="$1" -v a="$2" 'NR == 1 { gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); printf "add-generic-password -U -s %s -a %s -w \"%s\"\n", s, a, $0 }' |
    /usr/bin/perl -e 'alarm 20; exec @ARGV' /usr/bin/env -i HOME="$home" PATH=/usr/bin:/bin /usr/bin/security -i >/dev/null
}

secrets() {
  local tokfile=$root/run/faketg.token
  if [[ ! -s $tokfile ]]; then
    (umask 077 && printf '1000001:hommy-fake-s%s-%s\n' "$slot" "$(openssl rand -hex 8)" >"$tokfile")
  fi
  put_secret "$svc" telegram-bot-token <"$tokfile"
  # The build Mac's Claude test token (ops/claude-token.sh import), when present: copied over stdin, never shown.
  if ! hm_real_keychain_unlocked; then
    echo "  the build Mac's keychain is locked: Claude test token not copied (no dialog raised)"
  elif security find-generic-password -s hommy-test-claude-token -a hommy >/dev/null 2>&1; then
    security find-generic-password -s hommy-test-claude-token -a hommy -w | put_secret hommy-test-claude-token hommy
    echo "  claude token copied into the scratch keychain"
  fi
}

seed_home() {
  local sock
  mkdir -p "$home/Library/LaunchAgents" "$home/Library/Application Support/agterm" "$root/vault" "$root/logs" "$root/run"
  python3 - "$home/.claude.json" <<'PY'
import json, os, sys
p = sys.argv[1]
d = {}
if os.path.exists(p):
    try:
        d = json.load(open(p))
    except ValueError:
        d = {}
if d.get("hasCompletedOnboarding") is not True:
    d["hasCompletedOnboarding"] = True
    with open(p, "w") as f:
        json.dump(d, f, indent=2)
PY
  sock="${HOMMY_AGTERM_SOCKET:-$HOME/Library/Application Support/agterm/agterm.sock}"
  if [[ -S $sock && $sock != "$home"/* ]]; then
    ln -sfn "$sock" "$home/Library/Application Support/agterm/agterm.sock"
  fi
}

start_services() {
  local name port busy
  while read -r name port; do
    hm_svc_pid "$root" "$name" >/dev/null && continue
    case $name in
      faketg) set -- faketg --addr "127.0.0.1:$port" --token-file "$root/run/faketg.token" ;;
      fakespeech) set -- fakespeech --addr "127.0.0.1:$port" --fixtures "$HM_REPO/e2e/fixtures/audio" ;;
      static-site) set -- static-site --addr "127.0.0.1:$port" --dir "$HM_REPO/e2e/fixtures/static" ;;
    esac
    "$HM_REPO/scripts/sx.sh" --slot "$slot" python3 "$HM_REPO/scripts/lib/detach.py" \
      --pid-file "$root/run/$name.pid" --log "$root/logs/$name.log" -- "$HM_REPO/bin/hommy-e2e" "$@"
    if ! hm_wait_http "$(svc_url "$name" "$port")"; then
      busy=$(tail -3 "$root/logs/$name.log" 2>/dev/null || true)
      hm_die "$name did not answer on :$port; log tail: $busy"
    fi
  done < <(services)
}

cmd_up() {
  local busy p
  if [[ -f $root/run/layout && $(cat "$root/run/layout") != "$layout" ]] && any_running; then
    hm_die "slot $slot is up with another layout: scripts/test-env.sh down first"
  fi
  mkdir -p "$root/run"
  chmod 700 "$root"
  echo "$layout" >"$root/run/layout"
  if ((start)) && all_up; then
    print_ports
    echo ready
    return 0
  fi
  ((start)) && any_running && stop_all
  busy=$( (hm_listening "$B" $((B + 9)); hm_listening $((B + 10000)) $((B + 10009))) | tr '\n' ' ')
  if [[ -n ${busy// /} ]]; then
    for p in $busy; do printf 'test-env: port %s is in use\n' "$p" >&2; done
    exit 3
  fi
  [[ -x $HM_REPO/bin/hommy-e2e ]] || "$HM_REPO/scripts/dev-build.sh" >/dev/null
  local claude
  claude=$(hm_resolve_claude) || HM_DIE_CODE=3 hm_die "cannot resolve the real claude by absolute path (set HOMMY_CLAUDE_BIN)"
  mkdir -p "$home"
  hm_link_bin "$root" "$claude"
  seed_home
  ensure_keychain
  secrets
  ((start)) && start_services
  print_ports
  echo ready
}

cmd_down() {
  local left
  stop_all
  if [[ -d $root ]] && ((purge)); then
    [[ $root == */hm-s[0-9]* ]] || hm_die "refusing to purge $root"
    remove_keychain
    rm -rf "$root"
    echo "purged $root"
  fi
  left=$( (hm_listening "$B" $((B + 9)); hm_listening $((B + 10000)) $((B + 10009))) | tr '\n' ' ')
  if [[ -n ${left// /} ]]; then
    printf 'test-env: still listening on the slot (not ours?): %s\n' "$left" >&2
    exit 1
  fi
  echo down
}

cmd_status() {
  local name port state=down n=0 up=0 listening nl
  while read -r name port; do
    n=$((n + 1))
    if hm_svc_pid "$root" "$name" >/dev/null; then up=$((up + 1)); fi
  done < <(services)
  if ((up == n && n > 0)) && all_up; then state=up; elif ((up > 0)); then state=partial; fi
  echo "$state"
  while read -r name port; do
    printf '  %-12s :%s %s\n' "$name" "$port" "$(hm_svc_pid "$root" "$name" >/dev/null && echo running || echo stopped)"
  done < <(services)
  listening=$(hm_listening "$B" $((B + 9)) | tr '\n' ' ')
  printf '  %-12s %s\n' listening "${listening:-none} ($B-$((B + 9)))"
  nl=$(labels | grep -c . || true)
  printf '  %-12s %s %s* labels loaded\n' launchd "$nl" "$(hm_label_prefix "$slot")"
  printf '  %-12s %s (%s)\n' root "$root" "$([[ -d $root ]] && echo present || echo absent)"
  [[ $state == up ]]
}

case $cmd in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  home) echo "$home" ;;
  *) usage ;;
esac
