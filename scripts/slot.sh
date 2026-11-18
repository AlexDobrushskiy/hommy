#!/bin/bash
# Workslots (docs/workslots.md): extra checkouts for parallel lanes. Slot N ≥ 1 is a git worktree of origin/main at
# <main checkout>-slots/slot<N> on its own branch slot<N>/<slug>, with ports 4710+10N … +9 and scratch root
# /tmp/hm-s<N>/. Slot 0 is the phase checkout: shown, never mutated.
#   scripts/slot.sh status [N]                   state (phase|free|taken), branch, ports, scratch root
#   scripts/slot.sh list                         N, state, branch, dir per existing slot
#   scripts/slot.sh claim SLUG [--slot N]        a new worktree on slot<N>/<slug> (N: the lowest free); prints its dir
#   scripts/slot.sh release N [--force]          stops its test env, removes the worktree, its branch and scratch root
#   scripts/slot.sh path N                       the slot's checkout dir
# release refuses a dirty tree or commits that are neither in origin/main nor pushed, unless --force.
set -euo pipefail
HM_ME=slot
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

usage() { sed -n '2,11s/^# \{0,1\}//p' "$0" >&2; exit 2; }
MAIN=$(dirname "$(git -C "$HM_REPO" rev-parse --path-format=absolute --git-common-dir)")
SLOTS=$MAIN-slots

slot_dir() { if (($1 == 0)); then echo "$MAIN"; else echo "$SLOTS/slot$1"; fi; }
need_n() {
  if ! [[ ${1:-} =~ ^[0-9]+$ ]] || (($1 > HM_SLOT_MAX)); then hm_die "want a slot number 0..$HM_SLOT_MAX, not '${1:-}'"; fi
}
need_mutable() {
  need_n "$1"
  (($1 >= 1)) || hm_die "slot 0 is the phase checkout: not a workslot"
}
is_worktree() { git -C "$MAIN" worktree list --porcelain | grep -qxF "worktree $(slot_dir "$1")"; }
# The full ref, not --short: a same-named tag makes --short print heads/<b>, which `git branch -D` can't find.
branch_of() {
  local ref
  ref=$(git -C "$(slot_dir "$1")" symbolic-ref --quiet HEAD 2>/dev/null) || { echo "(detached)"; return 0; }
  echo "${ref#refs/heads/}"
}
state_of() {
  if (($1 == 0)); then echo phase; elif [[ -e $(slot_dir "$1") ]]; then echo taken; else echo free; fi
}
dirty() { [[ -n $(git -C "$(slot_dir "$1")" status --porcelain 2>/dev/null) ]]; }

cmd_status() {
  local list n b
  if [[ -n ${1:-} ]]; then need_n "$1"; list=$1; else list="0 $(cmd_list | cut -f1 | tr '\n' ' ')"; fi
  for n in $list; do
    b=$(hm_base_port "$n")
    printf 'slot %s: %s\n' "$n" "$(state_of "$n")"
    [[ $(state_of "$n") == free ]] && continue
    printf '  %-8s %s\n' dir "$(slot_dir "$n")" branch "$(branch_of "$n")$(dirty "$n" && echo ' (dirty)' || true)" \
      ports "$b-$((b + 9))" scratch "$(hm_root "$n")/"
  done
}

cmd_list() {
  local d n
  for d in "$SLOTS"/slot*; do
    [[ -e $d ]] || continue
    n=${d##*/slot}
    [[ $n =~ ^[0-9]+$ ]] || continue
    printf '%s\t%s\t%s\t%s\n' "$n" "$(state_of "$n")" "$(branch_of "$n")" "$d"
  done | sort -n
}

lock() {
  mkdir -p "$SLOTS"
  mkdir "$SLOTS/.lock" 2>/dev/null || hm_die "another slot operation is running (or a stale $SLOTS/.lock: remove it)"
  trap 'rmdir "$SLOTS/.lock" 2>/dev/null || true' EXIT
}

cmd_claim() {
  local slug=${1:-} n='' d branch
  [[ -n $slug && $slug != -* ]] || usage
  shift
  if [[ ${1:-} == --slot ]]; then n=${2:-}; need_mutable "$n"; shift 2; fi
  (($# == 0)) || usage
  [[ $slug =~ ^[a-z0-9][a-z0-9._-]*$ ]] || hm_die "slug: lower-case letters, digits, . _ - only"
  lock
  if [[ -z $n ]]; then
    for ((n = 1; n <= HM_SLOT_MAX; n++)); do [[ $(state_of "$n") == free ]] && break; done
    ((n <= HM_SLOT_MAX)) || hm_die "every slot 1..$HM_SLOT_MAX is taken"
  fi
  d=$(slot_dir "$n")
  [[ $(state_of "$n") == free ]] || hm_die "slot $n is taken ($d)"
  branch=slot$n/$slug
  git -C "$MAIN" show-ref --quiet "refs/heads/$branch" && hm_die "branch $branch already exists"
  git -C "$MAIN" fetch -q origin main
  git -C "$MAIN" worktree add -q --no-track -b "$branch" "$d" origin/main
  echo "slot $n claimed: branch $branch from origin/main, ports $(hm_base_port "$n")-$(($(hm_base_port "$n") + 9)), scratch root $(hm_root "$n")/" >&2
  echo "$d"
}

cmd_release() {
  local n=${1:-} force=${2:-} d b ahead
  need_mutable "$n"
  [[ -z $force || $force == --force ]] || usage
  d=$(slot_dir "$n")
  [[ -e $d ]] || { echo "slot $n: free"; return 0; }
  is_worktree "$n" || hm_die "$d is not a worktree of $MAIN: not touching it"
  lock
  b=$(branch_of "$n")
  if [[ $force != --force ]]; then
    dirty "$n" && hm_die "slot $n has uncommitted changes (release $n --force discards them)"
    git -C "$d" fetch -q origin
    ahead=$(git -C "$d" rev-list --count HEAD --not origin/main --remotes 2>/dev/null || echo 0)
    ((ahead == 0)) || hm_die "slot $n has $ahead commit(s) neither in origin/main nor pushed (push, or --force)"
  fi
  # Its own test env first (its scripts, its slot), then the checkout.
  if [[ -x $d/scripts/test-env.sh ]]; then
    HOMMY_SLOT=$n "$d/scripts/test-env.sh" down --slot "$n" --purge >/dev/null || hm_die "slot $n: test-env down failed"
  else
    HOMMY_SLOT=$n "$HM_REPO/scripts/test-env.sh" down --slot "$n" --purge >/dev/null || hm_die "slot $n: test-env down failed"
  fi
  git -C "$MAIN" worktree remove --force "$d"
  [[ $b == "(detached)" ]] || git -C "$MAIN" branch -q -D "$b" # a bare name: git branch only looks in refs/heads
  git -C "$MAIN" worktree prune
  echo "slot $n released: worktree and branch $b removed; free"
}

cmd=${1:-status}
(($#)) && shift
case $cmd in
  status) (($# <= 1)) || usage; cmd_status "${1:-}" ;;
  list) (($# == 0)) || usage; cmd_list ;;
  claim) cmd_claim "$@" ;;
  release) (($# >= 1 && $# <= 2)) || usage; cmd_release "$@" ;;
  path) (($# == 1)) || usage; need_n "$1"; slot_dir "$1" ;;
  *) usage ;;
esac
