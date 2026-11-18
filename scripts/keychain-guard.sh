#!/bin/bash
# Proves a run raised no keychain dialog and left the user's real keychain setup alone.
#   scripts/keychain-guard.sh start    record the real `list-keychains -d user` / `default-keychain -d user` (real HOME,
#                                      not sx) and the SecurityAgent pids, then sample SecurityAgent until `check`
#   scripts/keychain-guard.sh check    `keychain-guard: ok (…)`, or FAIL lines (exit 1): a new SecurityAgent process
#                                      appeared (= a keychain dialog), or the real search list / default changed
set -euo pipefail
HM_ME=keychain-guard
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

slot=$(hm_default_slot)
dir=/tmp/hm-kcguard-s$slot
real() { /usr/bin/env HOME="$(hm_real_home)" /usr/bin/security "$@" 2>&1; }

case ${1:-} in
  start)
    hm_svc_stop "$dir" sampler
    mkdir -p "$dir"
    chmod 700 "$dir"
    { real list-keychains -d user; real default-keychain -d user; } >"$dir/real.before"
    { /usr/bin/pgrep -x SecurityAgent || true; } >"$dir/agents.before"
    : >"$dir/agents.seen"
    hm_svc_start "$dir" sampler "$HM_REPO/scripts/lib/agent-sampler.sh" "$dir/agents.seen"
    echo "keychain-guard: watching" ;;
  check)
    [[ -f $dir/real.before ]] || hm_die "not started: scripts/keychain-guard.sh start"
    hm_svc_stop "$dir" sampler
    bad=0
    new=$(sort -u "$dir/agents.seen" | grep -vxF -f "$dir/agents.before" || true)
    if [[ -n $new ]]; then
      echo "keychain-guard: FAIL: a SecurityAgent (keychain dialog) appeared: pid $(tr '\n' ' ' <<<"$new")"
      bad=1
    fi
    if [[ $({ real list-keychains -d user; real default-keychain -d user; }) != "$(cat "$dir/real.before")" ]]; then
      echo "keychain-guard: FAIL: the real keychain search list or default changed"
      bad=1
    fi
    rm -rf "$dir"
    ((bad)) && exit 1
    echo "keychain-guard: ok (no SecurityAgent appeared; real search list and default unchanged)" ;;
  *) sed -n '2,6s/^# \{0,1\}//p' "$0" >&2; exit 2 ;;
esac
