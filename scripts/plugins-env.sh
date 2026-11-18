#!/bin/bash
# The slot's Claude Code plugins in its scratch HOME (phase 04; its own script so test-env.sh stays 01/02's):
# hommy-browsers from its origin/main as the slot's test install (layout A: lanes MCP B+6, identity B+7, Chrome debug
# B+8/B+9, pool 0, labels dev.hommy.test[-sN].browsers.*, docs/testing.md §10), then the plugins `hommy` (this tree),
# `hommy-browsers` (that install) and `retainer` (its public marketplace) through `scripts/sx.sh claude plugin …`.
#   scripts/plugins-env.sh up     [--slot N]   install or refresh (idempotent), then `ready`
#   scripts/plugins-env.sh down   [--slot N]   stop the browsers' services (install.sh --uninstall); plugins stay
#   scripts/plugins-env.sh status [--slot N]   the plugins and the browsers' services
#   scripts/plugins-env.sh hb-home [--slot N]  prints the browsers install (HB_HOME)
# HOMMY_BROWSERS_REPO: where hommy-browsers comes from (a git URL or path; default its GitHub repo, branch main).
# Needs `scripts/test-env.sh up --slot N` first. Log of the browsers install: /tmp/hm-s<N>/logs/plugins-env.log.
set -euo pipefail
HM_ME=plugins-env
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,12s/^# \{0,1\}//p' "$0" >&2; exit 2; }
cmd=${1:-}
[[ -n $cmd ]] || usage
shift
slot=$(hm_default_slot)
while (($#)); do
  case $1 in
    --slot) slot=${2:?--slot needs a number}; shift 2 ;;
    *) usage ;;
  esac
done
hm_check_slot "$slot"
root=$(hm_root "$slot")
home=$root/home
B=$(hm_base_port "$slot")
if ((slot == 0)); then inst="test"; else inst="test-s$slot"; fi
hb=$home/.local/share/hommy-$inst-browsers
src=$root/src/hommy-browsers
repo_url=${HOMMY_BROWSERS_REPO:-https://github.com/AlexDobrushskiy/hommy-browsers.git}
plog=$root/logs/plugins-env.log
sx=("$HM_REPO/scripts/sx.sh" --slot "$slot")

need_env() { [[ -d $home ]] || hm_die "slot $slot is not set up: scripts/test-env.sh up --slot $slot first"; }

# fetch: the source's main into $src (the build user's git: the repo may be private while hommy is built)
fetch() {
  if [[ -d $src/.git ]]; then
    git -C "$src" fetch -q --depth 1 origin main
    git -C "$src" reset -q --hard FETCH_HEAD
  else
    rm -rf "$src"
    mkdir -p "$(dirname "$src")"
    GIT_TERMINAL_PROMPT=0 git clone -q --depth 1 --branch main "$repo_url" "$src"
  fi
  git -C "$src" rev-parse --short HEAD
}

# install_browsers: the checkout into HB_HOME with the slot's config.env, then its own install.sh (idempotent)
install_browsers() {
  local tmp conf
  tmp=$(mktemp -d)
  git -C "$src" archive HEAD | tar -x -C "$tmp"
  mkdir -p "$hb"
  rsync -a --checksum --delete --exclude /node_modules/ --exclude /config.env --exclude /plugin/.mcp.json \
    --exclude /plugin/.hb-home --exclude /captcha/.venv/ --exclude __pycache__/ "$tmp/" "$hb/"
  conf=$tmp/.config.env
  cat >"$conf" <<CONF
# hommy test install, slot $slot (layout A); written by scripts/plugins-env.sh
HB_PORT_BASE=$((B + 6))
HB_DEBUG_BASE=$((B + 8))
HB_POOL=0
HB_LABEL_PREFIX=$(hm_label_prefix "$slot")browsers
HB_BUNDLE_ID=$(hm_label_prefix "$slot")agent-chrome
HB_STATE_DIR=$home/.local/state/hommy-$inst-browsers
HB_REAPER_SCOPE=own
CONF
  cmp -s "$conf" "$hb/config.env" || { cp "$conf" "$hb/config.env"; chmod 600 "$hb/config.env"; }
  rm -rf "$tmp"
  mkdir -p "$root/logs"
  if ! "${sx[@]}" "$hb/install.sh" >"$plog" 2>&1; then
    tail -15 "$plog" >&2
    hm_die "hommy-browsers install failed (full log: $plog)"
  fi
  grep -E '^changed: ' "$plog" | tail -1 | sed 's/^/  browsers: /'
}

installed() { "${sx[@]}" claude plugin list --json 2>/dev/null | jq -e --arg id "$1" 'any(.[]; .id == $id)' >/dev/null; }

has_marketplace() {
  "${sx[@]}" claude plugin marketplace list --json 2>/dev/null | jq -e --arg n "$1" 'any(.[]; .name == $n)' >/dev/null
}

# plugin ID MARKETPLACE SOURCE [--config k=v]…: add (or refresh) the marketplace, install the plugin once. A plugin
# from a local directory marketplace loads from that directory, so edits in the tree need no reinstall.
plugin() {
  local id=$1 mkt=$2 source=$3
  shift 3
  if has_marketplace "$mkt"; then
    "${sx[@]}" claude plugin marketplace update "$mkt" >/dev/null
  else
    "${sx[@]}" claude plugin marketplace add "$source" >/dev/null
  fi
  if installed "$id"; then
    printf '  %-32s installed\n' "$id"
  else
    "${sx[@]}" claude plugin install "$id" "$@" >/dev/null
    printf '  %-32s newly installed\n' "$id"
  fi
}

cmd_up() {
  need_env
  echo "hommy-browsers: $repo_url main @ $(fetch) → $hb"
  install_browsers
  echo "plugins (scratch HOME $home):"
  plugin hommy-browsers@hommy-browsers hommy-browsers "$hb" \
    --config "lanes_port=$((B + 6))" --config "identity_port=$((B + 7))" --config pool=0
  plugin hommy@hommy hommy "$HM_REPO" --config "instance=$inst"
  plugin retainer@retainer retainer AlexDobrushskiy/retainer
  echo ready
}

cmd_down() {
  if [[ -x $hb/install.sh && -d $home ]]; then
    "${sx[@]}" "$hb/install.sh" --uninstall | grep -E '^removed:' || true
  fi
  echo down
}

cmd_status() {
  need_env
  "${sx[@]}" claude plugin list --json | jq -r '.[] | "  \(.id) \(if .enabled then "enabled" else "disabled" end)"'
  launchctl list | awk -v p="$(hm_label_prefix "$slot")browsers" 'index($3, p) == 1 { print "  " $3, ($1 == "-" ? "stopped" : "pid " $1) }'
}

case $cmd in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  hb-home) echo "$hb" ;;
  *) usage ;;
esac
