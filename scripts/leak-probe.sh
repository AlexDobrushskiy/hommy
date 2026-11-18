#!/bin/bash
# Prove the leak gate end to end without touching this repo or GitHub (e2e/README.md › Leak gate).
#   scripts/leak-probe.sh commit [--token]   plant the canary word (or, with --token, a random bot-token-shaped
#                                            string) in docs/canary-probe.md and try to commit it: pre-commit must refuse
#   scripts/leak-probe.sh push               commit the canary with --no-verify (also in the message) and try to push
#                                            it to the probe's local bare remote: pre-push must refuse
#   scripts/leak-probe.sh pretooluse <hook command…>   stage the canary, then feed <hook command> Claude Code's
#                                            PreToolUse JSON for `git commit -m x` in the probe clone and print its
#                                            decision (a lane commit gate must answer `deny`)
# Clones HEAD into /tmp/hm-leakprobe-<pid>/ with a local bare remote (file://), copies this tree's hooks, gate files
# and clone config, prints the hooks' output and `result: refused|ACCEPTED`, and deletes the probe dir. Exit 0 when
# the gate refused, 1 when it let the canary through. Canaries come from scripts/leak/canary.sh and are never printed.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
mode=${1:-}; token=0
case $mode in commit | push | pretooluse) ;; *)
  echo "usage: scripts/leak-probe.sh commit [--token] | push | pretooluse <hook command…>" >&2; exit 2 ;;
esac
shift
[ "${1:-}" = --token ] && token=1
[ "$mode" = pretooluse ] && [ $# = 0 ] && { echo "leak-probe: pretooluse needs the hook command" >&2; exit 2; }
[ $token = 1 ] && [ "$mode" != commit ] && { echo "leak-probe: --token goes with commit" >&2; exit 2; }

P=/tmp/hm-leakprobe-$$
trap 'rm -rf "$P"' EXIT
rm -rf "$P"; mkdir -p "$P"
git clone -q --bare "$ROOT" "$P/remote.git"
git clone -q "file://$P/remote.git" "$P/work"
W=$P/work
# The gate as this working tree has it (it may not be committed yet).
for f in .githooks scripts/leak scripts/leak-scan.sh scripts/tools.sh scripts/tools.env .github/commit-identities \
  .gitleaks.toml; do
  rm -rf "${W:?}/$f"; mkdir -p "$(dirname "$W/$f")"; cp -R "$ROOT/$f" "$W/$f"
done
git -C "$W" config core.hooksPath .githooks
git -C "$W" config hommy.leak.required "$(git -C "$ROOT" config --get hommy.leak.required || echo false)"
git -C "$W" config user.name "$(git -C "$ROOT" config --get user.name || echo hommy)"
git -C "$W" config user.email "$(git -C "$ROOT" config --get user.email || echo noreply@github.com)"
export HOMMY_TOOLS_DIR=${HOMMY_TOOLS_DIR:-$ROOT/.dev/tools}
echo "probe dir: $P (clone of HEAD $(git -C "$ROOT" rev-parse --short=12 HEAD), bare remote file://$P/remote.git)"

mkdir -p "$W/docs"
tok=
if [ $token = 1 ]; then
  tok=$("$ROOT/scripts/leak/canary.sh" print-token)
  printf 'TELEGRAM_BOT_TOKEN=%s\n' "$tok" >"$W/docs/canary-probe.md"
  echo "planted: a bot-token-shaped string in docs/canary-probe.md:1"
else
  printf 'notes: %s\n' "$("$ROOT/scripts/leak/canary.sh" print-word)" >"$W/docs/canary-probe.md"
  echo "planted: the canary word in docs/canary-probe.md:1"
fi
git -C "$W" add docs/canary-probe.md

rc=0
if [ "$mode" = pretooluse ]; then
  json=$(python3 -c 'import json, sys; print(json.dumps({"hook_event_name": "PreToolUse", "tool_name": "Bash",
    "tool_input": {"command": "git commit -m x"}, "cwd": sys.argv[1]}))' "$W")
  echo "--- PreToolUse(Bash: git commit -m x, cwd=probe clone) -> $* ---"
  out=$(printf '%s' "$json" | (cd "$W" && "$@") 2>&1) || rc=$?
  decision=$(printf '%s' "$out" | python3 -c 'import json, sys
try:
    d = json.loads(sys.stdin.read())["hookSpecificOutput"]
    print(d["permissionDecision"]); print(d.get("permissionDecisionReason", ""), file=sys.stderr)
except Exception:
    print("allow (no decision)")' 2>"$P/reason") || true
  echo "decision: $decision"
  sed 's/^/reason: /' "$P/reason"
  echo "--- end ---"
  [ "$decision" = deny ] && result=refused || result=ACCEPTED
elif [ "$mode" = commit ]; then
  before=$(git -C "$W" rev-parse HEAD)
  echo "--- git commit (hooks run) ---"
  out=$(cd "$W" && git commit -q -m "probe: leak gate" 2>&1) || rc=$?
  printf '%s\n' "$out"
  echo "--- end ---"
  [ "$(git -C "$W" rev-parse HEAD)" = "$before" ] && [ $rc != 0 ] && result=refused || result=ACCEPTED
else
  git -C "$W" switch -q -c probe/leak-gate
  git -C "$W" commit -q --no-verify -m "probe: $("$ROOT/scripts/leak/canary.sh" print-word)"
  echo "probe commit: $(git -C "$W" rev-parse --short=12 HEAD) (made with --no-verify; canary in the file and the message)"
  echo "--- git push origin probe/leak-gate (hooks run) ---"
  out=$(cd "$W" && git push -q origin probe/leak-gate 2>&1) || rc=$?
  printf '%s\n' "$out"
  echo "--- end ---"
  if git -C "$P/remote.git" rev-parse -q --verify refs/heads/probe/leak-gate >/dev/null; then
    echo "bare remote: probe branch PRESENT"; result=ACCEPTED
  else
    echo "bare remote: no probe branch"
    [ $rc != 0 ] && result=refused || result=ACCEPTED
  fi
fi
if [ -n "$tok" ]; then
  n=$(printf '%s\n' "$out" | grep -cF "$tok" || true)
  echo "planted token in the hook output: $n"
fi
rm -rf "$P"
echo "probe dir removed: $([ -e "$P" ] && echo no || echo yes)"
echo "result: $result"
[ "$result" = refused ]
