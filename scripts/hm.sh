#!/bin/bash
# `H` of the E2E rows: this checkout's bin/hommy as the slot's test instance, through sx (scratch HOME, clean env).
#   scripts/hm.sh [--slot N] ARGS…        = scripts/sx.sh --slot N bin/hommy --instance test[-sN] ARGS…
#   scripts/hm.sh [--slot N] --h2 ARGS…   the second host: HOME /tmp/hm-s<N>-h2/home, --instance test[-sN]-h2
# stdin passes through (`printf %s KEY | scripts/hm.sh --h2 setup --join … --key-stdin`).
# HOMMY_BIN: another hommy binary instead of bin/hommy (05's rows: the build install.sh installed, .dev/artifacts/hommy).
set -euo pipefail
HM_ME=hm
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

slot=$(hm_default_slot) h2=0
while (($#)); do
  case $1 in
    --slot) slot=${2:?--slot needs a number}; shift 2 ;;
    --h2) h2=1; shift ;;
    *) break ;;
  esac
done
hm_check_slot "$slot"
(($#)) || { sed -n '2,6s/^# \{0,1\}//p' "$0" >&2; exit 2; }
bin=${HOMMY_BIN:-$HM_REPO/bin/hommy}
[[ $bin == /* ]] || bin=$PWD/$bin
[[ -x $bin ]] || hm_die "no $bin (scripts/dev-build.sh builds bin/hommy)"
if ((h2)); then
  [[ -d $(hm_h2_root "$slot")/home ]] || hm_die "no second host: scripts/test-env.sh up --instance $(hm_h2_instance "$slot") --join"
  exec "$HM_REPO/scripts/sx.sh" --slot "$slot" --root "$(hm_h2_root "$slot")" "$bin" \
    --instance "$(hm_h2_instance "$slot")" "$@"
fi
exec "$HM_REPO/scripts/sx.sh" --slot "$slot" "$bin" --instance "$(hm_instance "$slot")" "$@"
