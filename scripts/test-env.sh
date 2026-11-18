#!/bin/bash
# The slot's test environment (docs/testing.md §2–3): scratch root /tmp/hm-s<N>/ (HOME, bin links, keychain, logs),
# test secrets in the scratch keychain, the fakes started detached on the slot's ports, and (layout A) the test
# instance's config, Claude hooks file and vault (e2e/README.md › Test instance).
#   scripts/test-env.sh up     [--slot N] [--layout browsers] [--no-start] [--tg real]   prints the ports, then `ready`
#   scripts/test-env.sh up     [--slot N] --instance test[-sN]-h2 --join   second host: own scratch HOME; no hub, fakes, token
#   scripts/test-env.sh down   [--slot N] [--purge] | --instance test[-sN]-h2   stops everything; --purge removes the root
#   scripts/test-env.sh status [--slot N]                                    `up`, `partial` or `down` (exit 0 only for up)
#   scripts/test-env.sh home   [--slot N]                                    prints the scratch HOME
#   scripts/test-env.sh secrets-grep [--slot N] FILE…   how many FILEs hold one of the instance's Keychain secrets
#   scripts/test-env.sh host-key [--slot N]             prints the instance's host key (a scratch test secret)
# Exit 3: a port of the slot is in use (named).
set -euo pipefail
HM_ME=test-env
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,13s/^# \{0,1\}//p' "$0" >&2; exit 2; }

cmd=${1:-}
[[ -n $cmd ]] || usage
shift
slot=$(hm_default_slot) layout=a start=1 purge=0 tg=fake instance='' join=0
files=()
while (($#)); do
  case $1 in
    --slot) slot=${2:?--slot needs a number}; shift 2 ;;
    --layout) [[ ${2:-} == browsers ]] || hm_die "--layout takes only 'browsers'"; layout=b; shift 2 ;;
    --no-start) start=0; shift ;;
    --purge) purge=1; shift ;;
    --tg) [[ ${2:-} == real ]] || hm_die "--tg takes only 'real'"; tg=real; shift 2 ;;
    --instance) instance=${2:?--instance needs a name}; shift 2 ;;
    --join) join=1; shift ;;
    -*) usage ;;
    *) [[ $cmd == secrets-grep ]] || usage; files+=("$1"); shift ;;
  esac
done
hm_check_slot "$slot"
root=$(hm_root "$slot")
B=$(hm_base_port "$slot")
inst=$(hm_instance "$slot")
svc=$(hm_service "$slot")
host_name=''
# A second-host instance (`--instance <instance>-<name>`) has its own scratch root /tmp/hm-s<N>-<name>.
if [[ -n $instance && $instance != "$inst" ]]; then
  host_name=${instance#"$inst"-}
  if [[ $instance != "$inst"-* || ! $host_name =~ ^[a-z0-9]{1,8}$ ]]; then
    hm_die "--instance must be $inst-<name> (e.g. $(hm_h2_instance "$slot")), not $instance"
  fi
  [[ $cmd == down || ($cmd == up && $join == 1) ]] || hm_die "--instance $instance goes with \`up --join\` or \`down\`"
  root=$(hm_root "$slot")-$host_name inst=$instance svc=hommy-$instance
elif ((join)); then
  hm_die "--join needs --instance $(hm_h2_instance "$slot")"
fi
home=$root/home
kc=$(hm_keychain "$root")

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

# labels [PREFIX]: the loaded launchd labels of the slot's test agents (or of PREFIX: a second host's).
labels() { launchctl list 2>/dev/null | awk -v p="${1:-$(hm_label_prefix "$slot")}" 'index($3, p) == 1 { print $3 }'; }

# stop_labels [PREFIX]: boots out only dev.hommy.test… labels (the slot's, or PREFIX's).
stop_labels() {
  local label
  while read -r label; do
    [[ $label == dev.hommy.test* ]] || continue
    launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  done < <(labels "$@")
}

stop_all() {
  local name
  for name in faketg fakespeech static-site; do hm_svc_stop "$root" "$name"; done
  stop_labels
  stop_labels "dev.hommy.$(hm_h2_instance "$slot")."
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
  if [[ $tg == real ]]; then
    # The real test bot's token (testing.md §12): copied over stdin, never shown.
    hm_real_keychain_unlocked || hm_die "the build Mac's keychain is locked: cannot copy the test bot token (no dialog raised)"
    security find-generic-password -s hommy-test-bot-token -a hommy >/dev/null 2>&1 ||
      hm_die "no hommy-test-bot-token in the build Mac's keychain (docs/testing.md §12)"
    security find-generic-password -s hommy-test-bot-token -a hommy -w | put_secret "$svc" telegram-bot-token
    echo "  real test bot token copied into the scratch keychain"
  else
    put_secret "$svc" telegram-bot-token <"$tokfile"
  fi
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
      faketg) set -- faketg --addr "127.0.0.1:$port" --token-file "$root/run/faketg.token" --state-file "$root/run/faketg.ids" ;;
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
  local busy p running=0
  if [[ -f $root/run/layout && $(cat "$root/run/layout") != "$layout" ]] && any_running; then
    hm_die "slot $slot is up with another layout: scripts/test-env.sh down first"
  fi
  mkdir -p "$root/run"
  chmod 700 "$root"
  echo "$layout" >"$root/run/layout"
  if ((start)) && all_up; then
    running=1
  else
    ((start)) && any_running && stop_all
    busy=$( (hm_listening "$B" $((B + 9)); hm_listening $((B + 10000)) $((B + 10009))) | tr '\n' ' ')
    if [[ -n ${busy// /} ]]; then
      for p in $busy; do printf 'test-env: port %s is in use\n' "$p" >&2; done
      exit 3
    fi
  fi
  [[ -x $HM_REPO/bin/hommy-e2e ]] || "$HM_REPO/scripts/dev-build.sh" >/dev/null
  prepare_root
  secrets
  ((start && !running)) && start_services
  [[ $layout == a ]] && write_instance
  print_ports
  echo ready
}

# The scratch root's HOME, bin links, agterm socket link and keychain (the slot's or a second host's).
prepare_root() {
  local claude
  claude=$(hm_resolve_claude "$root") || HM_DIE_CODE=3 hm_die "cannot resolve the real claude by absolute path (set HOMMY_CLAUDE_BIN)"
  mkdir -p "$home"
  hm_link_bin "$root" "$claude"
  seed_home
  ensure_keychain
}

# set_config KEY=VALUE|-KEY…: the instance's hommy.env (0600, KEY=VALUE lines) with these keys set (-KEY: removed);
# every other line is kept (rows and `hommy setup` add their own keys).
set_config() {
  local f=$home/.config/hommy-$inst/hommy.env
  mkdir -p "$(dirname "$f")"
  chmod 700 "$(dirname "$f")"
  python3 - "$f" "$@" <<'PY'
import os, sys
path, edits = sys.argv[1], sys.argv[2:]
lines = open(path).read().splitlines() if os.path.exists(path) else []
drop = {e[1:] for e in edits if e.startswith("-")}
sets = dict(e.split("=", 1) for e in edits if not e.startswith("-"))
out, seen = [], set()
for line in lines:
    key = line.split("=", 1)[0].strip() if "=" in line and not line.lstrip().startswith("#") else None
    if key in drop or key in seen:
        continue
    if key in sets:
        out.append(f"{key}={sets[key]}")
        seen.add(key)
        continue
    out.append(line)
out += [f"{k}={v}" for k, v in sets.items() if k not in seen]
tmp = path + ".tmp"
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    f.write("\n".join(out) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, path)
PY
}

# The keys every test instance shares: the fakes' speech, the stub, the real-claude agent command, the agterm workspace.
common_keys() {
  printf '%s\n' "PORT_BASE=$B" "WHISPER_ENDPOINTS=fake|http://127.0.0.1:$((B + 2))/v1|whisper" \
    "TTS_URL=http://127.0.0.1:$((B + 2))/v1" SPEECH=external "AGTERM_WORKSPACE=hm-test-s$slot" \
    "CLAUDE_BIN=$(hm_root "$slot")/bin/claude-stub" \
    "AGENT_COMMAND=$HM_REPO/scripts/claude-session.sh --slot $slot interactive"
}

seed_vault() {
  [[ -e $root/vault/CLAUDE.md ]] && return 0
  mkdir -p "$root/vault"
  cp -R "$HM_REPO/e2e/fixtures/vault-min/." "$root/vault/"
}

# The slot's instance: config, vault, and the Claude hooks file claude-session.sh passes with --settings.
write_instance() {
  local keys=() k tg_key=-TELEGRAM_API_URL
  [[ $tg == real ]] || tg_key=TELEGRAM_API_URL=http://127.0.0.1:$((B + 1))
  while IFS= read -r k; do keys+=("$k"); done < <(common_keys)
  set_config "${keys[@]}" "$tg_key"
  seed_vault
  python3 - "$root/claude-hooks.json" "$home/.local/share/hommy-$inst/bin/hommy" "$inst" <<'PY'
import json, os, sys
path, exe, inst = sys.argv[1:]
events = {"SessionStart": "session-start", "UserPromptSubmit": "user-prompt-submit", "Stop": "stop",
          "Notification": "notify"}
hooks = {ev: [{"hooks": [{"type": "command", "command": f"{exe} --instance {inst} hook {arg}"}]}]
         for ev, arg in events.items()}
with open(path + ".tmp", "w") as f:
    json.dump({"hooks": hooks}, f, indent=2)
    f.write("\n")
os.replace(path + ".tmp", path)
PY
  printf '  %-12s %s\n' instance "$inst ($home/.config/hommy-$inst/hommy.env)" vault "$root/vault" hooks "$root/claude-hooks.json"
}

# The second host (02-E15): own scratch HOME, keychain and vault; config pointing at the slot's hub. No hub, fakes,
# token or service.
cmd_join() {
  local keys=() k
  mkdir -p "$root/run"
  chmod 700 "$root"
  prepare_root
  while IFS= read -r k; do keys+=("$k"); done < <(common_keys)
  set_config "${keys[@]}" "HUB_URL=ws://127.0.0.1:$B/ws" "HOST_NAME=$host_name"
  seed_vault
  printf 'instance %s · scratch root %s/\n' "$inst" "$root"
  printf '  %-12s %s\n' HOME "$home" hub "ws://127.0.0.1:$B/ws" vault "$root/vault"
  echo ready
}

cmd_join_down() {
  stop_labels "dev.hommy.$inst."
  if [[ -d $root ]]; then
    [[ $root == */hm-s[0-9]*-* ]] || hm_die "refusing to remove $root"
    remove_keychain
    chmod -R u+w "$root" 2>/dev/null || true # Go's module cache in the scratch HOME is read-only
    rm -rf "$root"
  fi
  echo "removed $root"
}

# read_secret ITEM: the instance's Keychain secret on stdout (callers never print it, except host-key).
read_secret() {
  [[ -d $root/home ]] || hm_die "slot $slot is not set up: scripts/test-env.sh up"
  "$HM_REPO/scripts/sx.sh" --slot "$slot" /usr/bin/security find-generic-password -s "$svc" -a "$1" -w 2>/dev/null
}

# The token, the host key and every key in hub-host-keys (`name key` entries, comma-separated) as fixed needles:
# prints only how many of FILEs hold one. A missing FILE or no secret at all is an error, never a clean 0.
cmd_secrets_grep() {
  local f needles n
  ((${#files[@]})) || usage
  for f in "${files[@]}"; do [[ -e $f ]] || HM_DIE_CODE=2 hm_die "nothing to scan: $f"; done
  needles=$( (read_secret telegram-bot-token || true; echo; read_secret host-key || true; echo
    (read_secret hub-host-keys || true) | tr ',' '\n' | awk 'NF >= 2 { print $NF }') | grep -v '^$' || true)
  [[ -n $needles ]] || hm_die "no secrets in the instance's keychain (service $svc)"
  n=$(printf '%s\n' "$needles" | "$HM_REPO/scripts/grep-tree.sh" "${files[@]}") || hm_die "grep failed"
  echo "${n% files}"
}

cmd_host_key() {
  read_secret host-key || hm_die "no host-key in the instance's keychain (service $svc): run setup first"
}

cmd_down() {
  local left
  stop_all
  if [[ -d $root ]] && ((purge)); then
    [[ $root == */hm-s[0-9]* ]] || hm_die "refusing to purge $root"
    remove_keychain
    chmod -R u+w "$root" 2>/dev/null || true # Go's module cache in the scratch HOME is read-only
    rm -rf "$root"
    [[ ! -e $root ]] || hm_die "could not remove $root"
    echo "purged $root"
    [[ ! -d $(hm_h2_root "$slot") ]] || "$0" down --slot "$slot" --instance "$(hm_h2_instance "$slot")"
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
  up) if [[ -n $host_name ]]; then cmd_join; else cmd_up; fi ;;
  down) if [[ -n $host_name ]]; then cmd_join_down; else cmd_down; fi ;;
  secrets-grep) cmd_secrets_grep ;;
  host-key) cmd_host_key ;;
  status) cmd_status ;;
  home) echo "$home" ;;
  *) usage ;;
esac
