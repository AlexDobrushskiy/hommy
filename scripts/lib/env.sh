# shellcheck shell=bash
# Shared by the harness scripts: slots, ports, scratch roots, the real tools, detached services (docs/testing.md).
# Source it after setting nothing; it defines HM_REPO (this checkout) and hm_* helpers. No side effects.

HM_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
HM_PORT_BASE=${HOMMY_PORT_BASE:-4710} # APP_PORT_BASE
HM_PORT_STEP=10                       # SLOT_PORT_STEP
HM_SLOT_MAX=9                         # slot 9 = 4800-4809, the end of the range

hm_die() { printf '%s: %s\n' "${HM_ME:-hommy}" "$*" >&2; exit "${HM_DIE_CODE:-1}"; }

# The slot this checkout owns: HOMMY_SLOT, else N for a workslot checkout <main>-slots/slotN, else 0.
hm_default_slot() {
  if [[ -n ${HOMMY_SLOT:-} ]]; then echo "$HOMMY_SLOT"; return; fi
  local base parent
  base=$(basename "$HM_REPO") parent=$(basename "$(dirname "$HM_REPO")")
  if [[ $base =~ ^slot([0-9]+)$ && $parent == *-slots ]]; then echo "${BASH_REMATCH[1]}"; else echo 0; fi
}

hm_check_slot() {
  if ! [[ $1 =~ ^[0-9]+$ ]] || (($1 > HM_SLOT_MAX)); then hm_die "slot must be 0..$HM_SLOT_MAX, not '$1'"; fi
}

hm_base_port() { echo $((HM_PORT_BASE + HM_PORT_STEP * $1)); }

# The scratch root: /tmp/hm-s<N> (HOMMY_SCRATCH_BASE replaces /tmp for unit tests).
hm_root() { echo "${HOMMY_SCRATCH_BASE:-/tmp}/hm-s$1"; }

# The instance's Keychain service in the scratch keychain: hommy-test (slot 0), hommy-test-s<N>.
hm_service() { if (($1 == 0)); then echo hommy-test; else echo "hommy-test-s$1"; fi; }

# The launchd label prefix of the slot's test agents.
hm_label_prefix() { if (($1 == 0)); then echo dev.hommy.test.; else echo "dev.hommy.test-s$1."; fi; }

# The real claude by absolute path: HOMMY_CLAUDE_BIN, else the first `claude` on PATH that is a Mach-O executable
# (shell-script shims in front of it are skipped).
hm_resolve_claude() {
  local p dir
  if [[ -n ${HOMMY_CLAUDE_BIN:-} ]]; then
    [[ $HOMMY_CLAUDE_BIN == /* && -x $HOMMY_CLAUDE_BIN ]] || return 1
    echo "$HOMMY_CLAUDE_BIN"
    return 0
  fi
  local IFS=:
  for dir in $PATH; do
    p=$dir/claude
    [[ $dir == /* && -x $p && ! -d $p ]] || continue
    [[ $dir == */hm-s[0-9]*/bin ]] && continue
    if /usr/bin/file -bL "$p" 2>/dev/null | grep -q '^Mach-O'; then
      echo "$p"
      return 0
    fi
  done
  return 1
}

# hm_link_bin ROOT CLAUDE: ROOT/bin links to the real claude, uv, node, npm, git, go (whichever exist).
hm_link_bin() {
  local root=$1 claude=$2 bin=$1/bin t p
  mkdir -p "$bin"
  [[ $(readlink "$bin/claude" 2>/dev/null) == "$claude" ]] || ln -sfn "$claude" "$bin/claude"
  for t in uv node npm git go; do
    p=$(PATH=$(hm_path_without "$bin") command -v "$t" 2>/dev/null) || continue
    [[ $p == /* ]] || continue
    [[ $(readlink "$bin/$t" 2>/dev/null) == "$p" ]] || ln -sfn "$p" "$bin/$t"
  done
  if [[ -x $HM_REPO/bin/hommy-e2e ]]; then
    [[ $(readlink "$bin/claude-stub" 2>/dev/null) == "$HM_REPO/bin/hommy-e2e" ]] || ln -sfn "$HM_REPO/bin/hommy-e2e" "$bin/claude-stub"
  fi
  [[ -d $root ]]
}

hm_path_without() {
  local out='' dir IFS=:
  for dir in $PATH; do [[ $dir == "$1" ]] || out=${out:+$out:}$dir; done
  echo "$out"
}

# Ports in [FROM, TO] that something listens on (TCP, any address), one per line.
hm_listening() {
  { /usr/sbin/lsof -nP -iTCP:"$1-$2" -sTCP:LISTEN -Fn 2>/dev/null || true; } | sed -n 's/^n.*:\([0-9][0-9]*\)$/\1/p' | sort -un
}

# --- detached services: ROOT/run/<name>.pid holds "<pid> <start time>" (scripts/lib/detach.py) ---

hm_svc_pid() { # prints the pid when the service runs (the recorded start time must match: no reused pid)
  local f=$1/run/$2.pid pid start now
  [[ -f $f ]] || return 1
  read -r pid start <"$f" || return 1
  now=$(ps -o lstart= -p "$pid" 2>/dev/null | xargs) || return 1
  [[ -n $now && $now == "$start" ]] || return 1
  echo "$pid"
}

hm_svc_start() { # ROOT NAME CMD…: start detached, logs in ROOT/logs/NAME.log
  local root=$1 name=$2
  shift 2
  mkdir -p "$root/run" "$root/logs"
  python3 "$HM_REPO/scripts/lib/detach.py" --pid-file "$root/run/$name.pid" --log "$root/logs/$name.log" -- "$@"
}

hm_svc_stop() { # ROOT NAME: TERM, then KILL after 5 s
  local root=$1 name=$2 pid i
  pid=$(hm_svc_pid "$root" "$name") || { rm -f "$root/run/$name.pid"; return 0; }
  kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < 50; i++)); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
  rm -f "$root/run/$name.pid"
}

hm_wait_http() { # URL: up to 10 s for an HTTP answer
  local i
  for ((i = 0; i < 100; i++)); do
    curl -fsS -m 1 -o /dev/null "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# Runs /usr/bin/security for the scratch HOME, never longer than 20 s (a locked keychain would wait on a dialog).
hm_scratch_security() {
  local home=$1
  shift
  /usr/bin/perl -e 'alarm 20; exec @ARGV or exit 127' /usr/bin/env -i HOME="$home" PATH=/usr/bin:/bin /usr/bin/security "$@"
}

hm_keychain() { echo "$1/home/Library/Keychains/hommy-test.keychain-db"; }

# The one way into the scratch keychain: unlock it from ROOT/kc.pw (`unlock-keychain -p` never asks the user) and
# re-apply "no auto-lock, no lock on sleep", before anything reads or writes it. A keychain that cannot be unlocked
# fails here, fast, instead of raising macOS's unlock dialog later. Never call show-keychain-info, lock-keychain or an
# item read on a keychain that may be locked: on a locked keychain they raise that dialog. No keychain: a no-op.
hm_unlock_keychain() {
  local root=$1 kc
  kc=$(hm_keychain "$root")
  [[ -f $kc ]] || return 0
  [[ -s $root/kc.pw ]] || { echo "scratch keychain without $root/kc.pw: scripts/test-env.sh down --purge" >&2; return 1; }
  hm_scratch_security "$root/home" unlock-keychain -p "$(cat "$root/kc.pw")" "$kc" ||
    { echo "cannot unlock the scratch keychain $kc" >&2; return 1; }
  hm_scratch_security "$root/home" set-keychain-settings "$kc"
  python3 "$HM_REPO/scripts/lib/kc-status.py" "$kc" >/dev/null || { echo "the scratch keychain is still locked" >&2; return 1; }
}

# The build Mac's default keychain is unlocked (checked without any dialog): only then may a script read it.
hm_real_keychain_unlocked() { python3 "$HM_REPO/scripts/lib/kc-status.py" default >/dev/null 2>&1; }

# The user's real home (not a scratch HOME the caller may run under).
hm_real_home() { /usr/bin/dscl . -read "/Users/$(id -un)" NFSHomeDirectory | awk '{print $2}'; }
