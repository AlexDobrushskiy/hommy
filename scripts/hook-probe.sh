#!/bin/bash
# Drives the scratch HOME's installed hommy plugin hook the way Claude Code does (05-E10), without a Claude session:
#   scripts/hook-probe.sh turn [--slot N] --session ID --msg M --text TEXT
# pipes a UserPromptSubmit whose prompt starts `[Telegram · tg-M]`, then a Stop whose last_assistant_message is TEXT,
# through the installed plugin's bin/hommy-hook (CLAUDE_PLUGIN_ROOT and CLAUDE_PLUGIN_OPTION_INSTANCE as Claude Code
# sets them from the plugin's userConfig), under scripts/sx.sh. Prints one line per hook run: exit code, bytes of
# output, seconds. Exit 1 when a run exits non-zero, prints anything or takes 5 s or more.
set -euo pipefail
HM_ME=hook-probe
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,8s/^# \{0,1\}//p' "$0" >&2; exit 2; }
[[ ${1:-} == turn ]] || usage
shift
slot=$(hm_default_slot) session='' msg='' text=''
while (($#)); do
  case $1 in
    --slot) slot=${2:?}; shift 2 ;;
    --session) session=${2:?}; shift 2 ;;
    --msg) msg=${2:?}; shift 2 ;;
    --text) text=${2:?}; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n $session && $msg =~ ^[0-9]+$ && -n $text ]] || usage
hm_check_slot "$slot"
root=$(hm_root "$slot")
sx=("$HM_REPO/scripts/sx.sh" --slot "$slot")

# the plugin as Claude Code loads it: a directory marketplace's folder, else the cached copy
plugin_root=$("${sx[@]}" claude plugin list --json 2>/dev/null |
  jq -r 'map(select(.id == "hommy@hommy")) | .[0] | (.readFromFolder // .installPath // empty)')
[[ -n $plugin_root && -x $plugin_root/bin/hommy-hook ]] || hm_die "hommy@hommy is not installed in $root/home (install.sh or scripts/plugins-env.sh)"
inst=$(jq -r '.pluginConfigs["hommy@hommy"].options.instance // ""' "$root/home/.claude/settings.json" 2>/dev/null || true)

cwd=$root/vault
up=$(jq -cn --arg s "$session" --arg p "[Telegram · tg-$msg] probe turn" --arg c "$cwd" \
  '{session_id: $s, hook_event_name: "UserPromptSubmit", prompt: $p, cwd: $c}')
stop=$(jq -cn --arg s "$session" --arg t "$text" --arg c "$cwd" \
  '{session_id: $s, hook_event_name: "Stop", stop_hook_active: false, last_assistant_message: $t, cwd: $c}')

bad=0
hook() { # hook EVENT PAYLOAD
  local out t0 t1 rc=0
  t0=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
  out=$(printf '%s' "$2" | "${sx[@]}" /usr/bin/env CLAUDE_PLUGIN_ROOT="$plugin_root" CLAUDE_PLUGIN_OPTION_INSTANCE="$inst" \
    "$plugin_root/bin/hommy-hook" "$1" 2>&1) || rc=$?
  t1=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
  local secs
  secs=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.2f", b - a }')
  printf '%-20s exit %s, output %s bytes, %s s\n' "$1" "$rc" "${#out}" "$secs"
  if ((rc != 0)) || [[ -n $out ]] || awk -v s="$secs" 'BEGIN { exit !(s >= 5) }'; then bad=1; fi
}
echo "plugin $plugin_root (instance ${inst:-default})"
hook user-prompt-submit "$up"
hook stop "$stop"
exit "$bad"
