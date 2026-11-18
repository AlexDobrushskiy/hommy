#!/bin/bash
# hommy installer: one command wires the whole kit on an Apple-Silicon Mac (macOS 14+): Homebrew if missing, the
# third-party tools from their own sources, Claude Code only when missing, hommy, hommy-browsers for the instance,
# mlx-audio, then `hommy setup` (vault, speech, services, plugins, pairing, doctor). A re-run changes nothing; a run
# with a newer hommy upgrades it. Every tool keeps its own origin (install log: <state>/install.log).
#   curl -fsSL https://raw.githubusercontent.com/AlexDobrushskiy/hommy/main/install.sh | bash
#   ./install.sh [--instance NAME] [--port-base N] [--whisper-model small|medium|large-v3-turbo]
#                [--with-whatsapp] [--with-telegram-user] [--with-captcha] [--source DIR] [hommy setup flags…]
# --source DIR       local artifacts instead of GitHub: DIR/dist/ (a GoReleaser snapshot: archive + hommy.rb),
#                    DIR/hommy-browsers.tar.gz (a git checkout), DIR/app (the hommy repo: Brewfile, plugin marketplace)
# --brew-check-only  (build Macs) stages 2-3 only check each Brewfile entry (a command or an .app); brew never runs
# --hommy-binary P   (build Macs) stage 6 uses this hommy binary instead of the cask
# Every other flag goes to `hommy setup` (hommy setup --help). Exit: 0 ok, 1 a stage failed, 2 usage, 3 refused
# (not an Apple-Silicon Mac on macOS 14+, not an admin user, under 10 GB free, or a missing tool with --brew-check-only).
set -euo pipefail

# the script's own file, or empty when it arrives on stdin (`curl … | bash`)
SELF=${BASH_SOURCE[0]:-}
[[ -n $SELF && -f $SELF ]] || SELF=''
usage() {
  if [[ -n $SELF ]]; then sed -n '2,14s/^# \{0,1\}//p' "$SELF" >&2; else
    echo "usage: install.sh [--instance NAME] [--port-base N] [--whisper-model SIZE] [--with-whatsapp] [--with-telegram-user]" >&2
    echo "                  [--with-captcha] [--source DIR] [hommy setup flags…] (hommy setup --help)" >&2
  fi
  exit 2
}

# The whole run is one function: under `curl … | bash` bash reads all of it before the first command, so a child
# that reads stdin cannot swallow the rest of the script, and a truncated download runs nothing.
main() {
  INSTANCE=${HOMMY_INSTANCE:-} PORT_BASE='' WHISPER='' SOURCE='' BREW_CHECK_ONLY=0 HOMMY_BINARY=''
  WITH_WHATSAPP=0 WITH_TG_USER=0 WITH_CAPTCHA=0 NON_INTERACTIVE=0 TOKEN_STDIN=0
  SETUP_ARGS=()
  # hommy setup's flags that take no value; every other --flag passes on with its value
  SETUP_BOOLS=" --non-interactive --no-pair-wait --token-stdin --key-stdin --seed-context --dry-run "
  while (($#)); do
    # --name=value reads as --name value (setup's no-value flags keep an explicit =true|false and pass on whole)
    if [[ $1 == --*=* && $SETUP_BOOLS != *" ${1%%=*} "* ]]; then set -- "${1%%=*}" "${1#*=}" "${@:2}"; fi
    case $1 in
      --instance) INSTANCE=${2:?--instance needs a name}; shift 2 ;;
      --port-base) PORT_BASE=${2:?--port-base needs a number}; shift 2 ;;
      --whisper-model) WHISPER=${2:?--whisper-model needs a size}; shift 2 ;;
      --source) SOURCE=${2:?--source needs a directory}; shift 2 ;;
      --brew-check-only) BREW_CHECK_ONLY=1; shift ;;
      --hommy-binary) HOMMY_BINARY=${2:?--hommy-binary needs a path}; shift 2 ;;
      --with-whatsapp) WITH_WHATSAPP=1; shift ;;
      --with-telegram-user) WITH_TG_USER=1; shift ;;
      --with-captcha) WITH_CAPTCHA=1; shift ;;
      -h | --help) usage ;;
      --*=*) # a setup no-value flag with an explicit value; its true forms (Go's flag package) act like the bare flag
        case ${1#*=} in
          1 | t | T | true | TRUE | True)
            [[ ${1%%=*} == --non-interactive ]] && NON_INTERACTIVE=1
            [[ ${1%%=*} == --token-stdin ]] && TOKEN_STDIN=1 ;;
        esac
        SETUP_ARGS+=("$1"); shift ;;
      --*)
        [[ $1 == --non-interactive ]] && NON_INTERACTIVE=1
        [[ $1 == --token-stdin ]] && TOKEN_STDIN=1
        if [[ $SETUP_BOOLS == *" $1 "* ]]; then
          SETUP_ARGS+=("$1"); shift
        else
          [[ $# -ge 2 ]] || { echo "install: $1 needs a value" >&2; exit 2; }
          SETUP_ARGS+=("$1" "$2"); shift 2
        fi ;;
      *) echo "install: unexpected argument $1" >&2; usage ;;
    esac
  done
  [[ -z $INSTANCE || $INSTANCE =~ ^[a-z0-9-]{1,24}$ ]] || { echo "install: --instance $INSTANCE: want [a-z0-9-]{1,24}" >&2; exit 2; }
  if [[ -n $SOURCE ]]; then
    [[ -d $SOURCE ]] || { echo "install: --source $SOURCE is not a directory" >&2; exit 2; }
    SOURCE=$(cd "$SOURCE" && pwd -P)
  fi
  if [[ -n $HOMMY_BINARY ]]; then
    [[ -x $HOMMY_BINARY ]] || { echo "install: --hommy-binary $HOMMY_BINARY is not executable" >&2; exit 2; }
    HOMMY_BINARY=$(cd "$(dirname "$HOMMY_BINARY")" && pwd -P)/$(basename "$HOMMY_BINARY")
  fi
  ((BREW_CHECK_ONLY == 0)) || [[ -n $HOMMY_BINARY ]] || { echo "install: --brew-check-only needs --hommy-binary" >&2; exit 2; }

  # --token-stdin: the token is setup's alone. Every earlier stage runs with stdin from /dev/null (an installer child
  # that reads stdin would swallow it); setup reads it back from fd 3.
  if ((TOKEN_STDIN)); then exec 3<&0 </dev/null; fi

  REPO=AlexDobrushskiy/hommy
  BROWSERS_REPO=https://github.com/AlexDobrushskiy/hommy-browsers.git
  SPACY_WHEEL=https://github.com/explosion/spacy-models/releases/download/en_core_web_sm-3.8.0/en_core_web_sm-3.8.0-py3-none-any.whl
  BASE=hommy${INSTANCE:+-$INSTANCE}
  STATE=$HOME/.local/state/$BASE
  LOG=$STATE/install.log
  BREW_PREFIX=${HOMMY_BREW_PREFIX:-/opt/homebrew} # tests point it at a fake prefix
  export PATH="$BREW_PREFIX/bin:$HOME/.local/bin:$PATH" HOMEBREW_NO_ENV_HINTS=1
  CHANGES=0
  INST_ARGS=()
  [[ -n $INSTANCE ]] && INST_ARGS=(--instance "$INSTANCE")
  mkdir -p "$STATE"
  chmod 700 "$STATE"

  say() { printf '%s\n' "$*"; }
  # stage N NAME STATE DETAIL: the stage's one line; installed/upgraded/changed count as a change
  stage() {
    printf '[%s/9] %s: %s (%s)\n' "$1" "$2" "$3" "$4"
    case $3 in installed | upgraded | changed) CHANGES=$((CHANGES + 1)) ;; esac
  }
  # log STAGE COMMAND SOURCE EXIT: one install.log line (never a secret: commands and origins only)
  log() { printf '%s stage=%s command="%s" source=%s exit=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "$3" "$4" >>"$LOG"; }
  # note STAGE TEXT: a log line without an origin (a check, a skip, setup): only installs carry `source=`
  note() { printf '%s stage=%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" >>"$LOG"; }
  fail() { echo "install: $1" >&2; note "${2:-?}" "failed: $1"; exit "${3:-1}"; }
  # run STAGE SOURCE CMD…: runs CMD, logs it with its origin, fails the stage on a non-zero exit
  run() {
    local st=$1 src=$2 rc=0
    shift 2
    "$@" || rc=$?
    log "$st" "$*" "$src" "$rc"
    ((rc == 0)) || fail "stage $st: $* exited $rc" "$st"
  }

  note 0 "install.sh start: instance=${INSTANCE:-default} artifacts=${SOURCE:-github} brew-check-only=$BREW_CHECK_ONLY"

  # --- 1. preflight ---
  arch=$(uname -m)
  os=$(sw_vers -productVersion 2>/dev/null || echo 0)
  [[ $arch == arm64 ]] || fail "hommy needs a Mac with Apple Silicon (arm64), this is $arch" 1 3
  ((${os%%.*} >= 14)) || fail "hommy needs macOS 14 or later, this is $os" 1 3
  id -Gn | tr ' ' '\n' | grep -qx admin || fail "run install.sh as an admin user (Homebrew needs one)" 1 3
  free=$(df -g "$HOME" | awk 'NR == 2 { print $4 }')
  ((free >= 10)) || fail "under 10 GB free on $HOME ($free GB)" 1 3
  stage 1 preflight present "macOS $os $arch, admin, $free GB free"

  # --- 2. Homebrew ---
  brew_bin() { if [[ -x $BREW_PREFIX/bin/brew ]]; then echo "$BREW_PREFIX/bin/brew"; else command -v brew || true; fi; }
  BREW=$(brew_bin)
  if [[ -n $BREW ]]; then
    stage 2 Homebrew present "$BREW"
  elif ((BREW_CHECK_ONLY)); then
    fail "Homebrew is missing (--brew-check-only installs nothing)" 2 3
  else
    say "Homebrew is missing: its official installer runs now (it asks for your password once)."
    tmp=$(mktemp -d)
    curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh -o "$tmp/brew-install.sh" ||
      fail "cannot download Homebrew's installer" 2
    if ((NON_INTERACTIVE)); then
      run 2 https://github.com/Homebrew/install env NONINTERACTIVE=1 /bin/bash "$tmp/brew-install.sh"
    elif { : </dev/tty; } 2>/dev/null; then
      # the terminal, not install.sh's stdin (under `curl … | bash` that is the script itself): Homebrew asks there
      run 2 https://github.com/Homebrew/install /bin/bash "$tmp/brew-install.sh" </dev/tty
    else
      run 2 https://github.com/Homebrew/install /bin/bash "$tmp/brew-install.sh"
    fi
    rm -rf "$tmp"
    BREW=$(brew_bin)
    [[ -n $BREW ]] || fail "Homebrew's installer finished but brew is not there" 2
    stage 2 Homebrew installed "from Homebrew/install"
  fi
  ((BREW_CHECK_ONLY)) || eval "$("$BREW" shellenv)"

  # trust STAGE KIND NAME: Homebrew 7 loads a formula or cask from a non-official tap only once it is trusted; trust exactly
  # the entry this installer installs (never a whole tap). Older Homebrew has no `brew trust` and needs none.
  trust() {
    [[ $3 == */*/* ]] || return 0
    "$BREW" trust --help >/dev/null 2>&1 || return 0
    "$BREW" trust "--$2" "$3" >/dev/null 2>&1 || fail "brew trust --$2 $3 failed" "$1"
    note "$1" "brew trust --$2 $3"
  }

  # --- 3. Brewfile ---
  brewfile() {
    local here=''
    [[ -n $SELF ]] && here=$(cd "$(dirname "$SELF")" && pwd -P)
    if [[ -n $SOURCE && -f $SOURCE/app/Brewfile ]]; then echo "$SOURCE/app/Brewfile"
    elif [[ -n $here && -f $here/Brewfile ]]; then echo "$here/Brewfile"
    else
      local f
      f=$(mktemp)
      curl -fsSL "https://raw.githubusercontent.com/$REPO/main/Brewfile" -o "$f" || fail "cannot download the Brewfile" 3
      echo "$f"
    fi
  }
  BREWFILE=$(brewfile)
  have_app() { [[ -d ${HOMMY_APPLICATIONS:-/Applications}/$1 || -d $HOME/Applications/$1 ]]; }
  # missing_entries: "kind name check" for every Brewfile entry whose check fails
  missing_entries() {
    local kind name check
    while read -r kind name check; do
      if [[ $check == *.app ]]; then have_app "$check" && continue; else command -v "$check" >/dev/null 2>&1 && continue; fi
      echo "$kind $name $check"
    done < <(sed -nE 's/^(brew|cask) "([^"]+)".*# check: (.+)$/\1 \2 \3/p' "$BREWFILE" | sed -E 's/[[:space:]]+$//')
  }
  missing=$(missing_entries)
  entries=$(grep -cE '^(brew|cask) ' "$BREWFILE")
  if [[ -z $missing ]]; then
    stage 3 "Brewfile" present "$entries entries: $(sed -nE 's/^(brew|cask) "([^"]+)".*/\2/p' "$BREWFILE" | sed 's#.*/##' | tr '\n' ' ' | sed 's/ $//')"
  elif ((BREW_CHECK_ONLY)); then
    while read -r _ name check; do echo "install: missing: $name ($check not found)" >&2; done <<<"$missing"
    fail "Brewfile: missing $(awk '{print $2}' <<<"$missing" | tr '\n' ' ')(--brew-check-only installs nothing)" 3 3
  else
    # one source= line per tool it installs (below), not one for the bundle itself
    while read -r kind name _; do trust 3 "$([[ $kind == cask ]] && echo cask || echo formula)" "$name"; done <<<"$missing"
    # bundle only the missing entries (and the taps they come from): brew bundle judges "installed" by Homebrew's own
    # records, so a present tool installed another way (Chrome from google.com, git from the CLT) would be installed again
    need=$(mktemp)
    sed -nE 's/^tap "([^"]+)".*/\1/p' "$BREWFILE" | while read -r tap; do
      if awk -v t="$tap/" 'index($2, t) == 1 { found = 1 } END { exit !found }' <<<"$missing"; then echo "tap \"$tap\""; fi
    done >"$need"
    while read -r _ name _; do
      awk -v q="\"$name\"" '($1 == "brew" || $1 == "cask") && $2 == q' "$BREWFILE"
    done <<<"$missing" >>"$need"
    rc=0
    "$BREW" bundle --file "$need" --no-upgrade || rc=$?
    note 3 "brew bundle --no-upgrade ($(grep -cE '^(brew|cask) ' "$need") missing of $entries entries in $BREWFILE) exit=$rc"
    rm -f "$need"
    ((rc == 0)) || fail "stage 3: brew bundle exited $rc" 3
    while read -r kind name _; do
      case $kind:$name in
        cask:*/*) log 3 "brew install --cask $name" "Homebrew cask (${name%/*} tap)" 0 ;;
        cask:*) log 3 "brew install --cask $name" "Homebrew cask (homebrew/cask)" 0 ;;
        brew:*/*/*) log 3 "brew install $name" "Homebrew (${name%/*} tap)" 0 ;;
        *) log 3 "brew install $name" "Homebrew (homebrew/core)" 0 ;;
      esac
    done <<<"$missing"
    still=$(missing_entries)
    [[ -z $still ]] || fail "Brewfile: still missing after brew bundle: $(awk '{print $2}' <<<"$still" | tr '\n' ' ')" 3
    stage 3 "Brewfile" installed "$(awk '{print $2}' <<<"$missing" | sed 's#.*/##' | tr '\n' ' ' | sed 's/ $//')"
  fi

  # --- 4. mlx-audio (Kokoro text-to-speech) ---
  UV=$(command -v uv || true)
  [[ -n $UV ]] || fail "uv not found (Brewfile)" 4
  if "$UV" tool list 2>/dev/null | grep -q '^mlx-audio '; then
    stage 4 mlx-audio present "$("$UV" tool list 2>/dev/null | grep '^mlx-audio ' | head -1)"
  else
    run 4 "PyPI (mlx-audio, misaki) + spaCy release (en_core_web_sm) + uv-managed Python 3.12" \
      "$UV" tool install --python 3.12 "mlx-audio[server,tts]" --with "misaki[en]" --with "$SPACY_WHEEL"
    stage 4 mlx-audio installed "$("$UV" tool list 2>/dev/null | grep '^mlx-audio ' | head -1) in $HOME/.local/share/uv/tools"
  fi

  # --- 5. Claude Code (only when missing; never bundled) ---
  claude_bin() { command -v claude 2>/dev/null || { [[ -x $HOME/.local/bin/claude ]] && echo "$HOME/.local/bin/claude"; } || true; }
  CLAUDE=$(claude_bin)
  if [[ -n $CLAUDE ]]; then
    stage 5 "Claude Code" present "$CLAUDE"
  else
    tmp=$(mktemp -d)
    curl -fsSL https://claude.ai/install.sh -o "$tmp/claude-install.sh" || fail "cannot download Claude Code's installer" 5
    run 5 https://claude.ai/install.sh bash "$tmp/claude-install.sh"
    rm -rf "$tmp"
    CLAUDE=$(claude_bin)
    [[ -n $CLAUDE ]] || fail "Claude Code's installer finished but claude is not there" 5
    stage 5 "Claude Code" installed "$CLAUDE via claude.ai/install.sh; sign in when setup asks"
  fi

  # --- 6. hommy ---
  version_of() { { "$1" version 2>/dev/null || true; } | awk '{print $2}'; }
  SIGNED=$HOME/.local/share/$BASE/bin/hommy
  # local_tap: the generated cask from DIR/dist/, pointing at the local archive (before the public tap exists)
  local_tap() {
    local tapdir cask archive v
    [[ -f $SOURCE/dist/hommy.rb ]] || fail "$SOURCE/dist/hommy.rb missing (scripts/vm-artifacts.sh builds it)" 6
    # the archive of the cask's own version (an older snapshot may sit beside it)
    v=$(sed -nE 's/^[[:space:]]*version "([^"]+)".*/\1/p' "$SOURCE/dist/hommy.rb" | head -n 1)
    archive=$SOURCE/dist/hommy_${v}_darwin_arm64.tar.gz
    [[ -n $v && -f $archive ]] || fail "no darwin/arm64 archive for version ${v:-?} in $SOURCE/dist" 6
    tapdir=$("$BREW" --repository)/Library/Taps/hommy-local/homebrew-tap
    if [[ ! -d $tapdir ]]; then
      "$BREW" tap-new hommy-local/tap --no-git >/dev/null || fail "brew tap-new hommy-local/tap failed" 6
      note 6 "brew tap-new hommy-local/tap --no-git (the local tap for $SOURCE/dist)"
    fi
    mkdir -p "$tapdir/Casks"
    cask=$(mktemp)
    sed -E "s#^([[:space:]]*)url \".*\"#\\1url \"file://$archive\"#" "$SOURCE/dist/hommy.rb" >"$cask"
    if ! cmp -s "$cask" "$tapdir/Casks/hommy.rb"; then cp "$cask" "$tapdir/Casks/hommy.rb"; fi
    rm -f "$cask"
  }
  if [[ -n $HOMMY_BINARY ]]; then
    HOMMY=$HOMMY_BINARY
    new=$(version_of "$HOMMY")
    [[ -n $new ]] || fail "$HOMMY does not run" 6
    old=''
    [[ -x $SIGNED ]] && old=$(version_of "$SIGNED")
    # the signed copy records the SHA-256 of the binary it was made from (hommy service install)
    stamp=$(cat "$(dirname "$SIGNED")/.hommy.sha256" 2>/dev/null || true)
    if [[ -z $old ]]; then
      log 6 "use $HOMMY" "local binary ($HOMMY)" 0
      stage 6 hommy installed "$new from the binary $HOMMY"
    elif [[ $(shasum -a 256 <"$HOMMY" | cut -c1-64) != "$stamp" ]]; then
      log 6 "use $HOMMY" "local binary ($HOMMY)" 0
      stage 6 hommy upgraded "$old → $new from the binary $HOMMY"
    else
      stage 6 hommy present "$new ($SIGNED)"
    fi
  else
    if [[ -n $SOURCE ]]; then
      local_tap
      TAP_CASK=hommy-local/tap/hommy SRC_DESC="local tap from $SOURCE/dist"
    else
      TAP_CASK=AlexDobrushskiy/tap/hommy SRC_DESC="the AlexDobrushskiy/tap cask"
    fi
    trust 6 cask "$TAP_CASK"
    if ! "$BREW" list --cask hommy >/dev/null 2>&1; then
      run 6 "$SRC_DESC" "$BREW" install --cask "$TAP_CASK"
      stage 6 hommy installed "$(version_of "$BREW_PREFIX/bin/hommy") from $SRC_DESC"
    # brew outdated exits 1 when something is outdated: read its output, not its status
    elif { "$BREW" outdated --cask --greedy hommy 2>/dev/null || true; } | grep -q hommy; then
      old=$(version_of "$BREW_PREFIX/bin/hommy")
      run 6 "$SRC_DESC" "$BREW" upgrade --cask "$TAP_CASK"
      stage 6 hommy upgraded "$old → $(version_of "$BREW_PREFIX/bin/hommy")"
    else
      stage 6 hommy present "$(version_of "$BREW_PREFIX/bin/hommy")"
    fi
    HOMMY=$BREW_PREFIX/bin/hommy
  fi
  H() { "$HOMMY" "${INST_ARGS[@]+"${INST_ARGS[@]}"}" "$@"; }
  # the instance's port base before its config exists (stage 7 derives the browsers' ports from it)
  if [[ -n $PORT_BASE ]]; then export PORT_BASE; fi

  # --- 7. hommy-browsers ---
  HB_HOME=$(H config get HB_HOME)
  hb_src=$BROWSERS_REPO hb_tmp='' hb_origin="GitHub (AlexDobrushskiy/hommy-browsers)"
  if [[ -n $SOURCE ]]; then
    [[ -f $SOURCE/hommy-browsers.tar.gz ]] || fail "$SOURCE/hommy-browsers.tar.gz missing" 7
    hb_tmp=$(mktemp -d)
    tar -xzf "$SOURCE/hommy-browsers.tar.gz" -C "$hb_tmp"
    hb_src=$hb_tmp/hommy-browsers hb_origin="local artifact ($SOURCE/hommy-browsers.tar.gz)"
  fi
  hb_state=present
  if [[ ! -d $HB_HOME/.git ]]; then
    [[ ! -e $HB_HOME ]] || [[ -z $(ls -A "$HB_HOME" 2>/dev/null) ]] || fail "$HB_HOME exists and is not a hommy-browsers checkout" 7
    mkdir -p "$(dirname "$HB_HOME")"
    run 7 "$hb_origin" git clone -q "$hb_src" "$HB_HOME"
    hb_state=installed
  else
    git -C "$HB_HOME" fetch -q "$hb_src" HEAD || fail "cannot fetch hommy-browsers from $hb_src" 7
    if [[ $(git -C "$HB_HOME" rev-parse HEAD) != $(git -C "$HB_HOME" rev-parse FETCH_HEAD) ]]; then
      run 7 "$hb_origin" git -C "$HB_HOME" merge -q --ff-only FETCH_HEAD
      hb_state=upgraded
    fi
  fi
  [[ -n $hb_tmp ]] && rm -rf "$hb_tmp"
  conf=$(H browsers configure) || fail "hommy browsers configure failed" 7
  [[ $conf == *"config.env: changed"* && $hb_state == present ]] && hb_state=changed
  hb_args=()
  ((WITH_CAPTCHA)) && hb_args+=(--with-captcha)
  hb_out=$(mktemp)
  if ! "$HB_HOME/install.sh" "${hb_args[@]+"${hb_args[@]}"}" >"$hb_out" 2>&1; then
    tail -15 "$hb_out" >&2
    log 7 "$HB_HOME/install.sh" "hommy-browsers ($HB_HOME)" 1
    fail "hommy-browsers' install.sh failed" 7
  fi
  n=$(sed -n 's/^changed: \([0-9]*\)$/\1/p' "$hb_out" | tail -1)
  rm -f "$hb_out"
  [[ ${n:-0} -gt 0 && $hb_state == present ]] && hb_state=changed
  stage 7 hommy-browsers "$hb_state" "$HB_HOME; $(sed -n 's/^ports: //p' <<<"$conf"); install.sh changed ${n:-0}"

  # --- 8. optional tools ---
  opt=()
  optional() { # optional FLAG CHECK KIND NAME SOURCE
    local flag=$1 check=$2 kind=$3 name=$4 src=$5
    ((flag)) || return 0
    if command -v "$check" >/dev/null 2>&1; then opt+=("$check present"); return 0; fi
    ((BREW_CHECK_ONLY)) && fail "$check missing (--brew-check-only installs nothing)" 8 3
    if [[ $kind == cask ]]; then
      trust 8 cask "$name"
      run 8 "$src" "$BREW" install --cask "$name"
    else
      trust 8 formula "$name"
      run 8 "$src" "$BREW" install "$name"
    fi
    opt+=("$check installed")
    CHANGES=$((CHANGES + 1))
  }
  optional "$WITH_WHATSAPP" wacli brew openclaw/tap/wacli "Homebrew (openclaw/tap)"
  optional "$WITH_WHATSAPP" shfmt brew shfmt "Homebrew (homebrew/core)"
  optional "$WITH_TG_USER" telegram-agent-cli cask AlexDobrushskiy/tap/telegram-agent-cli "Homebrew cask (AlexDobrushskiy/tap)"
  if ((${#opt[@]})); then
    local list=${opt[0]} o
    for o in "${opt[@]:1}"; do list+=", $o"; done
    printf '[8/9] optional: %s\n' "$list"
  else
    printf '[8/9] optional: skipped (--with-whatsapp, --with-telegram-user)\n'
  fi

  # --- 9. hommy setup ---
  setup=("$HOMMY" "${INST_ARGS[@]+"${INST_ARGS[@]}"}" setup)
  [[ -n $PORT_BASE ]] && setup+=(--port-base "$PORT_BASE")
  [[ -n $WHISPER ]] && setup+=(--whisper-model "$WHISPER")
  [[ -n $SOURCE && " ${SETUP_ARGS[*]-} " != *" --plugin-source "* ]] && setup+=(--plugin-source "$SOURCE/app")
  setup+=("${SETUP_ARGS[@]+"${SETUP_ARGS[@]}"}")
  out=$(mktemp)
  rc=0
  if ((TOKEN_STDIN)); then
    "${setup[@]}" <&3 2>&1 | tee "$out" || rc=${PIPESTATUS[0]}
    exec 3<&-
  elif ((NON_INTERACTIVE)) || ! { : </dev/tty; } 2>/dev/null; then
    "${setup[@]}" 2>&1 | tee "$out" || rc=${PIPESTATUS[0]}
  else
    "${setup[@]}" </dev/tty 2>&1 | tee "$out" || rc=${PIPESTATUS[0]}
  fi
  note 9 "hommy setup exit=$rc"
  sc=$(sed -n 's/^setup: \([0-9]*\) changes$/\1/p' "$out" | tail -1)
  rm -f "$out"
  ((rc == 0)) || fail "hommy setup exited $rc (its doctor lines above say what to fix; re-run install.sh after)" 9 "$rc"
  CHANGES=$((CHANGES + ${sc:-0}))
  if ((${sc:-0} > 0)); then printf '[9/9] hommy setup: changed (%s changes)\n' "$sc"; else printf '[9/9] hommy setup: present (0 changes)\n'; fi
  note 9 "install.sh done: $CHANGES changes"
  echo "install: $CHANGES changes (log: $LOG)"
}

main "$@"
