#!/bin/bash
# Scratch exec (docs/testing.md §3): runs CMD with `env -i` plus an allowlist (USER LOGNAME SHELL TERM LANG LC_*
# TMPDIR HOMMY_*), HOME=/tmp/hm-s<N>/home, PATH=/tmp/hm-s<N>/bin:/opt/homebrew/bin:/usr/bin:/bin and
# HOMEBREW_NO_AUTO_UPDATE=1. Nothing of the caller's Claude session (CLAUDE_CONFIG_DIR, CLAUDECODE, CLAUDE_CODE_*,
# ANTHROPIC_*, wrapper variables) passes. Refuses (exit 3) when the real claude cannot be resolved by absolute path.
#   scripts/sx.sh [--slot N] [--agterm-session ID] [--] CMD [ARGS…]
set -euo pipefail
HM_ME=sx
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

slot=$(hm_default_slot)
agterm=''
while (($#)); do
  case $1 in
    --slot) slot=${2:?--slot needs a number}; shift 2 ;;
    --agterm-session) agterm=${2:?--agterm-session needs an id}; shift 2 ;;
    --) shift; break ;;
    -h | --help) sed -n '2,7s/^# \{0,1\}//p' "$0"; exit 0 ;;
    *) break ;;
  esac
done
(($#)) || { sed -n '2,7s/^# \{0,1\}//p' "$0" >&2; exit 2; }
hm_check_slot "$slot"

claude=$(hm_resolve_claude) || HM_DIE_CODE=3 hm_die "cannot resolve the real claude by absolute path (no Mach-O \`claude\` on PATH; set HOMMY_CLAUDE_BIN)"
root=$(hm_root "$slot")
mkdir -p "$root/home"
chmod 700 "$root"
hm_link_bin "$root" "$claude"
# Whatever CMD is (security, hommy, the real claude), the scratch keychain is unlocked first: never a GUI prompt.
hm_unlock_keychain "$root" || HM_DIE_CODE=4 hm_die "scratch keychain not usable (see above)"

envs=("HOME=$root/home" "PATH=$root/bin:/opt/homebrew/bin:/usr/bin:/bin" HOMEBREW_NO_AUTO_UPDATE=1 "HOMMY_SLOT=$slot")
while IFS= read -r name; do
  case $name in
    USER | LOGNAME | SHELL | TERM | LANG | TMPDIR | LC_* | HOMMY_*)
      [[ $name == HOMMY_SLOT ]] || envs+=("$name=${!name}") ;;
  esac
done < <(compgen -e)
[[ -n $agterm ]] && envs+=("AGTERM_SESSION_ID=$agterm")
exec /usr/bin/env -i "${envs[@]}" "$@"
