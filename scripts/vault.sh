#!/bin/bash
# The vault template in a folder, until `hommy vault` (phase 05) does this from the binary.
#   scripts/vault.sh init <dir>                                   the template tree (vault-template/ minus embed.go)
#                                                                  into <dir>, which must be empty or absent (exit 3)
#   scripts/vault.sh add-place <vault> <name> <url> [--kind K] [--notes N]
#                                                                  a row `| name | kind | url | notes |` in
#                                                                  context/places.md (a row of the same name is replaced;
#                                                                  its kind and notes stay unless given)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
usage() { sed -n '2,8s/^# \{0,1\}//p' "$0" >&2; exit 2; }
die() { echo "vault: $1" >&2; exit "${2:-1}"; }

cmd_init() {
  [[ $# -eq 1 ]] || usage
  local dir=$1 tmp
  if [[ -e $dir ]]; then
    [[ -d $dir ]] || die "$dir is not a directory" 3
    [[ -z $(ls -A "$dir") ]] || die "$dir is not empty" 3
  fi
  mkdir -p "$dir"
  tmp=$(mktemp -d)
  # every template file except Go sources (embed.go, tests)
  (cd "$ROOT/vault-template" && find . -type f ! -name '*.go' ! -name '.DS_Store' -print0 | tar -c --null -T - -f -) |
    tar -x -C "$tmp" -f -
  cp -R "$tmp/." "$dir/"
  rm -rf "$tmp"
  echo "vault: initialized $(cd "$dir" && pwd -P) ($(find "$dir" -type f | wc -l | tr -d ' ') files)"
}

cmd_add_place() {
  [[ $# -ge 3 ]] || usage
  local vault=$1 name=$2 url=$3 kind='' notes='' f
  shift 3
  while (($#)); do
    case $1 in
      --kind) kind=${2:?--kind needs a value}; shift 2 ;;
      --notes) notes=${2:?--notes needs a value}; shift 2 ;;
      *) usage ;;
    esac
  done
  f=$vault/context/places.md
  [[ -f $f ]] || die "$f not found (scripts/vault.sh init first)"
  for v in "$name" "$kind" "$url" "$notes"; do
    [[ $v != *'|'* && $v != *$'\n'* ]] || die "values may not contain | or a newline"
  done
  [[ -n $name ]] || die "the name is empty"
  /usr/bin/python3 - "$f" "$name" "$kind" "$url" "$notes" <<'PY'
import sys

path, name, kind, url, notes = sys.argv[1:]
lines = open(path, encoding="utf-8").read().splitlines()
key = name.strip().lower()


def is_row(l):
    return l.lstrip().startswith("|")


# the places table: the first table whose header starts with | Name |
start = next((i for i, l in enumerate(lines) if is_row(l) and l.strip("| ").lower().startswith("name")), None)
if start is None:
    lines += ["", "## Places", "", "| Name | Kind | Booking | Notes |", "|---|---|---|---|",
              f"| {name} | {kind or '—'} | {url} | {notes or '—'} |"]
else:
    end = start
    while end + 1 < len(lines) and is_row(lines[end + 1]):
        end += 1
    same = [i for i in range(start + 2, end + 1) if lines[i].strip("| ").split("|")[0].strip().lower() == key]
    if same:
        old = [c.strip() for c in lines[same[0]].strip().strip("|").split("|")] + ["", "", "", ""]
        lines[same[0]] = f"| {name} | {kind or old[1] or '—'} | {url} | {notes or old[3] or '—'} |"
    else:
        lines.insert(end + 1, f"| {name} | {kind or '—'} | {url} | {notes or '—'} |")
open(path, "w", encoding="utf-8").write("\n".join(lines) + "\n")
PY
  echo "vault: $f: $name"
}

cmd=${1:-}
[[ -n $cmd ]] || usage
shift
case $cmd in
  init) cmd_init "$@" ;;
  add-place) cmd_add_place "$@" ;;
  -h | --help) sed -n '2,8s/^# \{0,1\}//p' "$0"; exit 0 ;;
  *) usage ;;
esac
