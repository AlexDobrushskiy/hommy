#!/bin/bash
# Proves .githooks/pre-push's main rule (S10) without touching this repo or GitHub: in a throwaway clone of HEAD under
# /tmp/hm-prepush-<pid>/ with a local bare remote and a stub `gh` on PATH (canned PR list and checks), it pushes to
# main: a forced non-fast-forward (even as a green PR head), a fast-forward that heads no open PR, one whose PR has a
# failing check, one whose PR has no checks, and one whose PR is all green. Prints `<case>: refused|ACCEPTED (want …)`
# per case and `result: ok|FAILED`; deletes the dir. Exit 0 only when every case got what it wants.
#   scripts/prepush-probe.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
P=/tmp/hm-prepush-$$
trap 'rm -rf "$P"' EXIT
rm -rf "$P"; mkdir -p "$P/stub"
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
# The stub answers `gh pr list …` with $P/prs and `gh pr checks …` with $P/checks (what gh prints after --jq).
cat >"$P/stub/gh" <<EOF
#!/bin/sh
case "\$1 \$2" in
  "pr list") cat "$P/prs" ;;
  "pr checks") cat "$P/checks" ;;
  *) echo "gh stub: unexpected \$*" >&2; exit 1 ;;
esac
EOF
chmod +x "$P/stub/gh"
export PATH=$P/stub:$PATH
echo "probe dir: $P (clone of HEAD $(git -C "$ROOT" rev-parse --short=12 HEAD), bare remote, stub gh)"

main0=$(git -C "$W" rev-parse origin/main)
fail=0
push() { # CASE WANT SHA [--force]: push SHA to the bare remote's main with the hooks on, then put main back
  local name=$1 want=$2 sha=$3 out rc=0 got
  shift 3
  out=$(cd "$W" && git push -q "$@" origin "$sha:refs/heads/main" 2>&1) || rc=$?
  got=refused
  if [ $rc = 0 ] && [ "$(git -C "$P/remote.git" rev-parse refs/heads/main)" = "$sha" ]; then got=ACCEPTED; fi
  printf '%s\n' "$out" | grep '^pre-push:' | sed 's/^/  /' || true
  echo "$name: $got (want $want)"
  [ "$got" = "$want" ] || fail=1
  git -C "$P/remote.git" update-ref refs/heads/main "$main0"
}

# A fast-forward: a commit on main with a synthetic file (made without hooks: the gate under test is pre-push).
mkdir -p "$W/docs"
printf 'probe\n' >"$W/docs/prepush-probe.md"
git -C "$W" add docs/prepush-probe.md
tree=$(git -C "$W" write-tree)
ff=$(git -C "$W" commit-tree -p "$main0" -m "probe: fast-forward" "$tree")
# A non-fast-forward: a commit on main's parent, pushed with --force.
nonff=$(git -C "$W" commit-tree -p "$main0~1" -m "probe: non-fast-forward" "$main0~1^{tree}")

printf '7 %s\n' "$nonff" >"$P/prs"; echo SUCCESS >"$P/checks"
push non-ff-green-pr refused "$nonff" --force
: >"$P/prs"; echo SUCCESS >"$P/checks"
push ff-no-pr refused "$ff"
printf '7 %s\n' "$ff" >"$P/prs"; printf 'SUCCESS\nFAILURE\n' >"$P/checks"
push ff-red-pr refused "$ff"
printf '7 %s\n' "$ff" >"$P/prs"; : >"$P/checks"
push ff-pr-no-checks refused "$ff"
printf '7 %s\n' "$ff" >"$P/prs"; printf 'SUCCESS\nSUCCESS\n' >"$P/checks"
push ff-green-pr ACCEPTED "$ff"

rm -rf "$P"
echo "probe dir removed: $([ -e "$P" ] && echo no || echo yes)"
if [ $fail = 0 ]; then echo "result: ok"; else echo "result: FAILED"; fi
[ $fail = 0 ]
