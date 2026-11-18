#!/bin/bash
# Real Claude Code for claude=real rows (docs/testing.md §6), through sx (scratch HOME: none of the maintainer's
# settings, skills, plugins or MCP servers) with the test token from the scratch keychain in its environment only.
#   scripts/claude-session.sh [--slot N] -p --cwd DIR --stream FILE "<prompt>" [-- CLAUDE ARGS…]
#       one print-mode run; the stream-json events go to FILE (e2e/README.md › Real Claude has the jq queries)
#   scripts/claude-session.sh [--slot N] interactive [CLAUDE ARGS…]
#       the AGENT_COMMAND of claude=real rows: interactive claude, keeps AGTERM_SESSION_ID. One hook source: the hommy
#       plugin when the scratch HOME has it installed (plugins-env.sh / install.sh), else --settings
#       /tmp/hm-s<N>/claude-hooks.json (test-env.sh's file) when that exists
#   scripts/claude-session.sh [--slot N] --audit FILE
#       prints only `token matches: <n>` over FILE, .dev/ and the scratch root (the token is read here, never shown)
set -euo pipefail
HM_ME='claude-session'
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,13s/^# \{0,1\}//p' "$0" >&2; exit 2; }
slot=$(hm_default_slot)
[[ ${1:-} == --slot ]] && { slot=${2:?--slot needs a number}; shift 2; }
hm_check_slot "$slot"
root=$(hm_root "$slot")
sx=("$HM_REPO/scripts/sx.sh" --slot "$slot")
with_token=$HM_REPO/scripts/lib/with-claude-token.sh

ready() {
  [[ -d $root/home ]] || hm_die "slot $slot is not set up: scripts/test-env.sh up"
  hm_unlock_keychain "$root" >/dev/null || hm_die "scratch keychain not usable (scripts/test-env.sh down --purge; up)"
}

case ${1:-} in
  -p)
    shift
    cwd='' stream='' prompt=''
    while (($#)); do
      case $1 in
        --cwd) cwd=${2:?}; shift 2 ;;
        --stream) stream=${2:?}; shift 2 ;;
        --) shift; break ;;
        *) [[ -z $prompt ]] || usage; prompt=$1; shift ;;
      esac
    done
    [[ -n $cwd && -n $stream && -n $prompt ]] || usage
    [[ -d $cwd ]] || hm_die "--cwd $cwd is not a directory"
    ready
    mkdir -p "$(dirname "$stream")"
    stream=$(cd "$(dirname "$stream")" && pwd -P)/$(basename "$stream")
    set +e
    (cd "$cwd" && printf '%s' "$prompt" | "${sx[@]}" "$with_token" claude -p --output-format stream-json --verbose "$@") >"$stream"
    rc=$?
    set -e
    printf 'claude-session: exit %s, %s events in %s\n' "$rc" "$(grep -c . "$stream" || true)" "$stream"
    exit "$rc" ;;
  interactive)
    shift
    ready
    extra=()
    # never both: with the plugin and the file, every hook event would reach the host twice
    if ! grep -q '"hommy@hommy"' "$root/home/.claude/plugins/installed_plugins.json" 2>/dev/null &&
      [[ -f $root/claude-hooks.json ]]; then
      extra=(--settings "$root/claude-hooks.json")
    fi
    [[ -n ${AGTERM_SESSION_ID:-} ]] && sx+=(--agterm-session "$AGTERM_SESSION_ID")
    exec "${sx[@]}" "$with_token" claude ${extra[@]+"${extra[@]}"} "$@" ;;
  --audit)
    file=${2:-}
    [[ -n $file ]] || usage
    ready
    targets=("$file" "$HM_REPO/.dev" "$root")
    # The scratch keychain's copy; the build Mac's item when the slot has none.
    if ! n=$("${sx[@]}" /usr/bin/security find-generic-password -s hommy-test-claude-token -a hommy -w 2>/dev/null |
      "$HM_REPO/scripts/grep-tree.sh" "${targets[@]}"); then
      hm_die "audit failed"
    fi
    if [[ $n == "0 files" ]] && ! "${sx[@]}" /usr/bin/security find-generic-password -s hommy-test-claude-token -a hommy >/dev/null 2>&1; then
      hm_real_keychain_unlocked || hm_die "the build Mac's keychain is locked: no token to audit with"
      n=$(security find-generic-password -s hommy-test-claude-token -a hommy -w 2>/dev/null | "$HM_REPO/scripts/grep-tree.sh" "${targets[@]}") ||
        hm_die "no token to audit with (../ops/claude-token.sh check)"
    fi
    echo "token matches: ${n% files}" ;;
  *) usage ;;
esac
