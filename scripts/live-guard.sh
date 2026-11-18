#!/bin/bash
# Proves a run left the maintainer's live setup alone (docs/testing.md §7).
#   scripts/live-guard.sh snapshot    record the state
#   scripts/live-guard.sh check       compare: `live-guard: ok (…)`, or one `live-guard: changed: <kind>` per difference (exit 1)
# The private config is $HOMMY_WORKSPACE/ops/live-guard.env, else ops/live-guard.env next to the main checkout; without
# it: `live-guard: no private config, skipped`. Prints counts only, never a label, path, hash or port.
set -euo pipefail
HM_ME=live-guard
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

main=$(dirname "$(git -C "$HM_REPO" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || echo "$HM_REPO/.git")")
conf=${HOMMY_WORKSPACE:+$HOMMY_WORKSPACE/ops/live-guard.env}
conf=${conf:-$(dirname "$main")/ops/live-guard.env}
if [[ ! -f $conf ]]; then
  echo "live-guard: no private config, skipped"
  exit 0
fi
LIVE_GUARD_LABELS='' LIVE_GUARD_FILES='' LIVE_GUARD_PORTS=''
# shellcheck disable=SC1090
source "$conf"
snap=/tmp/hm-live-guard-$(printf '%s' "$main" | shasum -a 256 | cut -c1-8).snap
real_home=$(hm_real_home)

state() {
  local uid label pid f p
  uid=$(id -u)
  if [[ -n $LIVE_GUARD_LABELS ]]; then
    launchctl list | awk 'NR > 1 { print $1, $3 }' | while read -r pid label; do
      [[ $label =~ $LIVE_GUARD_LABELS ]] || continue
      if launchctl print "gui/$uid/$label" 2>/dev/null | grep -q '^[[:space:]]*properties = .*keepalive'; then
        echo "keepalive $label $pid"
      else
        echo "label $label"
      fi
    done
  fi
  for f in $LIVE_GUARD_FILES; do
    if [[ -f $f ]]; then echo "file $f $(shasum -a 256 "$f" | cut -c1-64)"; else echo "file $f absent"; fi
  done
  for p in $LIVE_GUARD_PORTS; do
    if /usr/bin/nc -z -G 2 127.0.0.1 "$p" 2>/dev/null; then echo "port $p up"; else echo "port $p down"; fi
  done
  launchctl list | awk '$3 ~ /^dev\.hommy\./ && $3 !~ /^dev\.hommy\.test/ { print "devhommy", $3 }'
  for f in "$real_home"/.config/hommy* "$real_home"/Library/LaunchAgents/dev.hommy.*; do
    [[ -e $f ]] && echo "devhommy $f"
  done
  return 0
}

counts() {
  printf '%s labels, %s files, %s ports' "$(grep -cE '^(label|keepalive) ' <<<"$1" || true)" \
    "$(grep -c '^file ' <<<"$1" || true)" "$(grep -c '^port ' <<<"$1" || true)"
}

case ${1:-} in
  snapshot)
    now=$(state)
    (umask 077 && printf '%s\n' "$now" >"$snap")
    echo "live-guard: snapshot ($(counts "$now"))" ;;
  check)
    [[ -f $snap ]] || hm_die "no snapshot: scripts/live-guard.sh snapshot first"
    before=$(cat "$snap") now=$(state) bad=0
    kinds=$'labels\t^(label|keepalive) [^ ]*\nkeepalive pids\t^keepalive \nfiles\t^file \nports\t^port \ndev.hommy\t^devhommy '
    while IFS=$'\t' read -r kind re; do
      a=$(grep -E "$re" <<<"$before" | sort || true) b=$(grep -E "$re" <<<"$now" | sort || true)
      if [[ $kind == labels ]]; then
        a=$(grep -oE '^(label|keepalive) [^ ]+' <<<"$a" | sed 's/^keepalive /label /' | sort || true)
        b=$(grep -oE '^(label|keepalive) [^ ]+' <<<"$b" | sed 's/^keepalive /label /' | sort || true)
      fi
      if [[ $a != "$b" ]]; then echo "live-guard: changed: $kind"; bad=1; fi
    done <<<"$kinds"
    ((bad)) && exit 1
    echo "live-guard: ok ($(counts "$now"))" ;;
  *) sed -n '2,6s/^# \{0,1\}//p' "$0" >&2; exit 2 ;;
esac
