#!/bin/bash
# Tart VMs for the installer rows (docs/testing.md §8; Tart from scripts/tools.sh, never Homebrew).
#   scripts/vm.sh create NAME                  clone the cached image, start headless, key-based ssh, seed onboarding
#   scripts/vm.sh ssh NAME [SSH-OPTS…] [-- CMD…]   ssh (interactive without CMD); SSH-OPTS e.g. -L 14711:127.0.0.1:4711 -N -f
#   scripts/vm.sh copy NAME SRC DEST           rsync SRC into the VM (DEST relative to the VM's HOME)
#   scripts/vm.sh run NAME [--stdin] -- CMD…   CMD in the VM's HOME under `zsh -l` (exit code = CMD's)
#   scripts/vm.sh delete NAME                  stop + delete the VM and .dev/vm/NAME/ (never the cached image)
#   scripts/vm.sh ip|status NAME
# `run` and `ssh` unlock the VM's login keychain first. `run` also sets CLAUDE_CODE_OAUTH_TOKEN for CMD (and
# `launchctl setenv` in the VM) when the build Mac's default keychain is unlocked (checked without a dialog) and holds
# hommy-test-claude-token; the token travels on ssh's stdin only. CMD's stdin is empty unless --stdin forwards ours.
# Non-user steps (key install, keychain unlock, onboarding + agterm first-run seed, token env) go to .dev/vm/NAME/steps.log.
# Exit: 3 = refused (below 60 GB free, image not cached); otherwise the failing step's or CMD's code.
set -euo pipefail
HM_ME=vm
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"

IMAGE=ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest
VM_DISK_GB=${HOMMY_VM_DISK_GB:-80}
MIN_FREE_GB=60
VM_USER=admin VM_PASS=admin # the image's documented account (testing.md §8)
# Tart prunes its OCI cache to make room on clone/pull: the cached image must never go that way.
export TART_NO_AUTO_PRUNE=1

usage() { sed -n '2,14s/^# \{0,1\}//p' "$0" >&2; exit 2; }
(($# >= 2)) || usage
cmd=$1 name=$2
shift 2
[[ $name =~ ^[a-z0-9][a-z0-9-]{0,40}$ ]] || hm_die "VM name must be [a-z0-9-] (not an image name): $name"
tart=$("$HM_REPO/scripts/tools.sh" path tart 2>/dev/null) || {
  "$HM_REPO/scripts/tools.sh" ensure tart >/dev/null
  tart=$("$HM_REPO/scripts/tools.sh" path tart)
}
dir=$HM_REPO/.dev/vm/$name
key=$dir/id_ed25519 kh=$dir/known_hosts
# shellcheck disable=SC2016 # expands in the VM
UNLOCK='security unlock-keychain -p admin "$HOME/Library/Keychains/login.keychain-db"'

step() { mkdir -p "$dir"; printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>"$dir/steps.log"; }
exists() { "$tart" list --source local --quiet | grep -qx "$name"; }
state_of() {
  "$tart" list --source local --format json | python3 -c 'import json, sys
print(next((v["State"] for v in json.load(sys.stdin) if v["Name"] == sys.argv[1]), "absent"))' "$1"
}
running() { [[ $(state_of "$name") == running ]]; }
vm_ip() { "$tart" ip "$name" --wait "${1:-30}"; }
set_opts() {
  opts=(-F /dev/null -i "$key" -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile="$kh"
    -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR -o ServerAliveInterval=30 -o ConnectTimeout=10)
}
need_vm() {
  exists || hm_die "no VM $name (scripts/vm.sh create $name)"
  [[ -f $key ]] || hm_die "no ssh key for $name in $dir (a VM this script did not create?)"
  running || hm_die "VM $name is not running"
  ip=$(vm_ip) || hm_die "VM $name has no IP"
  set_opts
}
quote_args() { local a out=''; for a in "$@"; do out+=" $(printf '%q' "$a")"; done; printf '%s' "${out# }"; }
# shellcheck disable=SC2029 # the command line is quoted for the VM's shell on purpose
remote() { ssh "${opts[@]}" "$VM_USER@$ip" "$@"; }

create() {
  local free i v
  free=$(df -g "$HOME" | awk 'NR == 2 {print $4}')
  ((free >= MIN_FREE_GB)) || HM_DIE_CODE=3 hm_die "only ${free} GB free in $HOME, need $MIN_FREE_GB GB: refusing to create a VM"
  "$tart" list --source oci --quiet | grep -qxF "$IMAGE" ||
    HM_DIE_CODE=3 hm_die "$IMAGE is not in Tart's cache: refusing (this script never pulls it)"
  ! exists || hm_die "VM $name exists: scripts/vm.sh delete $name first"
  for v in $("$tart" list --source local --quiet); do
    [[ $(state_of "$v") != running ]] || hm_die "VM $v is running: one hommy VM at a time"
  done
  rm -rf "$dir"
  mkdir -p "$dir"
  chmod 700 "$dir"
  echo "clone $IMAGE -> $name (free: ${free} GB)"
  "$tart" clone "$IMAGE" "$name"
  # the image's disk leaves ~6 GB free: the kit needs more (Homebrew, Chrome, mlx-audio, the whisper model)
  "$tart" set "$name" --disk-size "$VM_DISK_GB"
  python3 "$HM_REPO/scripts/lib/detach.py" --pid-file "$dir/run/tart-run.pid" --log "$dir/tart-run.log" -- \
    "$tart" run --no-graphics --no-audio --no-clipboard "$name"
  ip=$(vm_ip 120) || hm_die "VM $name got no IP in 120 s (log: $dir/tart-run.log)"
  echo "ip: $ip"
  for ((i = 0; i < 90; i++)); do
    nc -z -G 2 "$ip" 22 >/dev/null 2>&1 && break
    sleep 2
  done
  nc -z -G 2 "$ip" 22 >/dev/null 2>&1 || hm_die "no ssh on $ip:22 after 180 s"

  # A throwaway key, installed with the image's password (its only use), then key-only ssh from here on.
  ssh-keygen -q -t ed25519 -N '' -C "hommy-vm-$name" -f "$key"
  : >"$kh"
  cat >"$dir/install-key.exp" <<'EXP'
set timeout 60
spawn -noecho ssh -F /dev/null -o UserKnownHostsFile=$env(VM_KH) -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR -o PubkeyAuthentication=no -o PreferredAuthentications=keyboard-interactive,password $env(VM_LOGIN) "mkdir -p .ssh && chmod 700 .ssh && echo '$env(VM_PUB)' >> .ssh/authorized_keys && chmod 600 .ssh/authorized_keys && echo key-installed"
expect {
  -re "assword.*:" { send "$env(VM_PASS)\r"; exp_continue }
  "key-installed" { }
  timeout { exit 1 }
  eof { exit 1 }
}
expect eof
catch wait result
exit [lindex $result 3]
EXP
  VM_KH=$kh VM_LOGIN=$VM_USER@$ip VM_PUB=$(cat "$key.pub") VM_PASS=$VM_PASS /usr/bin/expect "$dir/install-key.exp" >/dev/null ||
    hm_die "could not install the ssh key in $name"
  step "ssh: throwaway ed25519 key installed in $VM_USER's authorized_keys (password login once, by expect)"
  set_opts
  remote true </dev/null || hm_die "key-based ssh into $name failed"

  # run's helpers and Claude's onboarding flag: non-user steps.
  remote 'mkdir -p .hommy-vm && cat > .hommy-vm/run.zsh' <<'ZSH'
# scripts/vm.sh run: stdin line 1 = the Claude token or empty; the rest is CMD's stdin. Runs under zsh -l.
IFS= read -r t || t=''
security unlock-keychain -p admin "$HOME/Library/Keychains/login.keychain-db"
if [[ -n $t ]]; then
  export CLAUDE_CODE_OAUTH_TOKEN=$t
  # The GUI domain (agterm and its sessions), not this ssh session's: asuser needs root (the image's passwordless
  # sudo); the token goes on stdin, so sudo's log shows only the helper.
  print -r -- "$t" | sudo -n launchctl asuser "$(id -u)" /bin/zsh "$HOME/.hommy-vm/setenv.zsh" ||
    echo "vm run: launchctl setenv in the GUI domain failed" >&2
fi
unset t
export PATH=/opt/homebrew/bin:$HOME/.local/bin:$PATH
cd "$HOME"
"$@"
ZSH
  remote 'cat > .hommy-vm/setenv.zsh' <<'ZSH'
# scripts/vm.sh run: sets CLAUDE_CODE_OAUTH_TOKEN (stdin) for the launchd domain this runs in.
IFS= read -r v
launchctl setenv CLAUDE_CODE_OAUTH_TOKEN "$v"
ZSH
  step "harness: ~/.hommy-vm/run.zsh + setenv.zsh written (run's helpers)"
  remote 'test -f .claude.json || echo "{\"hasCompletedOnboarding\": true}" > .claude.json' </dev/null
  step "onboarding seed: ~/.claude.json {\"hasCompletedOnboarding\": true}"
  # agterm's two first-run prompts, answered as a user would. Launch 1 runs its modal Welcome alert, which blocks
  # every agtermctl reply; launch 2 shows Sparkle's "check for updates automatically?" prompt, and SwiftUI then
  # opens no main window, so the scene task that binds the control socket never runs.
  remote /bin/zsh -s <<'ZSH'
d="Library/Application Support/agterm"
mkdir -p "$d" && { test -f "$d/settings.json" || echo '{"welcomeShown": true}' >"$d/settings.json"; } &&
  defaults write com.umputun.agterm SUEnableAutomaticChecks -bool false
ZSH
  step "agterm first-run seed: settings.json {\"welcomeShown\": true} (Welcome alert) + defaults com.umputun.agterm SUEnableAutomaticChecks false (Sparkle prompt)"
  # macOS does not grow its APFS container into the larger disk by itself. repairDisk moves the recovery partition to
  # the disk's end; resizeContainer can still report -69519 after the container grew, so the size is what counts.
  local gb
  gb=$(remote 'yes | sudo -n diskutil repairDisk disk0 >/dev/null 2>&1; sudo -n diskutil apfs resizeContainer disk0s2 0 >/dev/null 2>&1; df -g /' </dev/null |
    awk 'NR == 2 { print $2 }')
  echo "disk: ${gb:-?} GB"
  ((${gb:-0} >= VM_DISK_GB - 15)) || hm_die "the VM's APFS container did not grow (${gb:-?} GB of $VM_DISK_GB)"
  step "disk: grown to $VM_DISK_GB GB (tart set --disk-size; diskutil apfs resizeContainer disk0s2 0)"
  echo "VM $name ready: $VM_USER@$ip (non-user steps: $dir/steps.log)"
}

# Prints run's first stdin line: the token when the build Mac's keychain is unlocked and holds it, else empty.
token_line() {
  if ! hm_real_keychain_unlocked; then
    step "token env: not set (the build Mac's keychain is locked; no dialog raised)"
    echo
  elif ! security find-generic-password -s hommy-test-claude-token -a hommy >/dev/null 2>&1; then
    step "token env: not set (no hommy-test-claude-token on the build Mac)"
    echo
  else
    step "token env: CLAUDE_CODE_OAUTH_TOKEN for this run + launchctl setenv in the VM's GUI domain (over ssh stdin)"
    security find-generic-password -s hommy-test-claude-token -a hommy -w
  fi
}

run() {
  local fwd=false rc line
  [[ ${1:-} == --stdin ]] && { fwd=true; shift; }
  [[ ${1:-} == -- ]] && shift
  (($#)) || usage
  need_vm
  step "keychain unlock: the VM's login keychain (run: $1)"
  line="/bin/zsh -l .hommy-vm/run.zsh $(quote_args "$@")"
  set +e
  if $fwd; then
    { token_line; cat; } | remote "$line"
    rc=${PIPESTATUS[1]}
  else
    token_line | remote "$line"
    rc=${PIPESTATUS[1]}
  fi
  set -e
  return "$rc"
}

do_ssh() {
  local extra=()
  while (($#)) && [[ $1 != -- ]]; do extra+=("$1"); shift; done
  [[ ${1:-} == -- ]] && shift
  need_vm
  step "keychain unlock: the VM's login keychain (ssh${1:+: $1})"
  remote "$UNLOCK" </dev/null
  if (($#)); then
    exec ssh "${opts[@]}" ${extra[@]+"${extra[@]}"} "$VM_USER@$ip" "$(quote_args "$@")"
  fi
  exec ssh "${opts[@]}" ${extra[@]+"${extra[@]}"} "$VM_USER@$ip"
}

copy() {
  (($# == 2)) || usage
  local src=$1 dest=$2
  [[ -e $src ]] || hm_die "no such file: $src"
  [[ $dest != /* ]] || hm_die "DEST is relative to the VM's HOME: $dest"
  need_vm
  rsync -a -e "ssh $(quote_args "${opts[@]}")" "$src" "$VM_USER@$ip:$dest"
  echo "copied $src -> $name:~/$dest"
}

delete() {
  local pid
  if exists; then
    if running; then "$tart" stop "$name" --timeout 30 >/dev/null 2>&1 || true; fi
    "$tart" delete "$name"
  fi
  if pid=$(hm_svc_pid "$dir" tart-run); then kill -TERM "$pid" 2>/dev/null || true; fi
  rm -rf "$dir"
  echo "deleted $name"
}

case $cmd in
  create) create ;;
  run) run "$@" ;;
  ssh) do_ssh "$@" ;;
  copy) copy "$@" ;;
  delete) delete ;;
  ip) exists || hm_die "no VM $name"; vm_ip ;;
  status)
    if ! exists; then echo absent; exit 0; fi
    if running; then echo "running $(vm_ip 5 || echo no-ip)"; else echo stopped; fi ;;
  *) usage ;;
esac
