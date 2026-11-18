#!/bin/bash
# Counts the files under DIR/FILE… that contain any needle read from stdin (one per line, fixed strings).
# Prints only `<n> files`: never a needle, a match or a file name.
#   printf '%s\n' NEEDLE… | scripts/grep-tree.sh DIR|FILE… [--exclude NAME]…
set -euo pipefail
targets=() excludes=()
while (($#)); do
  case $1 in
    --exclude) excludes+=(--exclude="${2:?--exclude needs a name}" --exclude-dir="$2"); shift 2 ;;
    -h | --help) sed -n '2,4s/^# \{0,1\}//p' "$0"; exit 0 ;;
    *) targets+=("$1"); shift ;;
  esac
done
((${#targets[@]})) || { sed -n '2,4s/^# \{0,1\}//p' "$0" >&2; exit 2; }
existing=()
for t in "${targets[@]}"; do [[ -e $t ]] && existing+=("$t"); done
if ((${#existing[@]} == 0)); then echo "0 files"; exit 0; fi
set +e
files=$(grep -v '^$' | grep -rlF -f /dev/stdin ${excludes[@]+"${excludes[@]}"} -- "${existing[@]}" 2>/dev/null)
rc=$?
set -e
n=0
[[ -n $files ]] && n=$(printf '%s\n' "$files" | grep -c .)
echo "$n files"
if ((rc > 1)); then
  echo "grep-tree: some files could not be read" >&2
  exit 1
fi
