#!/bin/bash
# The bundle `install.sh --source DIR` installs from (spec 05; the build Mac's scratch HOME and the Tart VM):
#   DIR/dist/                  release-snapshot: hommy_<V>_darwin_arm64.tar.gz, checksums.txt, hommy.rb (the cask)
#   DIR/hommy, DIR/VERSION     the binary from that archive (--hommy-binary) and its version (one line)
#   DIR/hommy-browsers.tar.gz  top dir hommy-browsers/: a full git clone of HOMMY_BROWSERS_SRC (default
#                              ../hommy-browsers) with its working-tree changes committed on top (never pushed)
#   DIR/app/                   this checkout's tracked + untracked-not-ignored files (plugin marketplace, fixtures)
#   DIR/install.sh, DIR/Brewfile   copies of the checkout's
#   DIR/fakes/hommy-e2e        the e2e fakes (faketg for the VM), darwin/arm64
# Usage: scripts/vm-artifacts.sh [--out DIR] [--version V]   (default DIR .dev/artifacts; V: release-snapshot's)
# DIR is rebuilt from scratch on every run.
set -euo pipefail
HM_ME=vm-artifacts
# shellcheck source-path=SCRIPTDIR source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
cd "$HM_REPO"

out=.dev/artifacts version=''
while (($#)); do
  case $1 in
    --out) out=${2:?--out DIR}; shift 2 ;;
    --version) version=${2:?--version V}; shift 2 ;;
    -h | --help) sed -n '2,12s/^# \{0,1\}//p' "$0"; exit 0 ;;
    *) hm_die "unknown argument: $1" ;;
  esac
done
hb_src=$(cd "${HOMMY_BROWSERS_SRC:-$HM_REPO/../hommy-browsers}" 2>/dev/null && pwd -P) ||
  hm_die "no hommy-browsers checkout at ${HOMMY_BROWSERS_SRC:-$HM_REPO/../hommy-browsers} (HOMMY_BROWSERS_SRC)"
git -C "$hb_src" rev-parse --verify -q HEAD >/dev/null || hm_die "$hb_src is not a git checkout with a commit"
noreply=$(grep -v '^#' .github/commit-identities | grep -v '^noreply@github.com$' | head -n 1)
[[ -n $noreply ]] || hm_die "no noreply identity in .github/commit-identities"

mkdir -p "$(dirname "$out")"
out=$(cd "$(dirname "$out")" && pwd -P)/$(basename "$out")
if [[ -d $out && ! -f $out/VERSION ]] && [[ -n $(ls -A "$out") ]]; then
  hm_die "$out exists and is not an artifacts dir (no VERSION): refusing to replace it"
fi
tmp=$(mktemp -d "${TMPDIR:-/tmp}/hm-artifacts.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
new=$tmp/out
mkdir -p "$new/dist" "$new/fakes"

# Copies the work-tree files git lists (tracked + untracked-not-ignored, NUL-separated on stdin) from SRC to DEST,
# skipping tracked files deleted from the work tree and anything under node_modules/.
copy_listed() {
  local src=$1 dest=$2 f
  while IFS= read -r -d '' f; do
    [[ $f == node_modules/* || $f == */node_modules/* ]] && continue
    [[ -e $src/$f || -L $src/$f ]] && printf '%s\0' "$f"
  done | rsync -a --from0 --files-from=- "$src/" "$dest/"
}

# 1. The release snapshot.
if [[ -n $version ]]; then
  HOMMY_VERSION=$version scripts/release-snapshot.sh >"$tmp/snapshot.log" 2>&1 || { cat "$tmp/snapshot.log" >&2; exit 1; }
else
  env -u HOMMY_VERSION scripts/release-snapshot.sh >"$tmp/snapshot.log" 2>&1 || { cat "$tmp/snapshot.log" >&2; exit 1; }
fi
version=$(sed -n 's/^version: //p' "$tmp/snapshot.log")
archive=dist/hommy_${version}_darwin_arm64.tar.gz
[[ -n $version && -f $archive && -f dist/hommy.rb && -f dist/checksums.txt ]] || hm_die "release-snapshot left no $archive"
cp "$archive" dist/checksums.txt dist/hommy.rb "$new/dist/"
mkdir -p "$tmp/x"
tar -xzf "$archive" -C "$tmp/x" hommy
mv "$tmp/x/hommy" "$new/hommy"
echo "$version" >"$new/VERSION"
echo "hommy $version: dist/ + hommy"

# 2. hommy-browsers: a full clone whose HEAD has the source's work tree (uncommitted changes included). Successive
# bundles must fast-forward (`git pull --ff-only` on upgrade), also when the work tree changed between builds without a
# commit, so every bundle's HEAD descends from the previous one: the local lineage repo .dev/hb-lineage.git keeps the
# last bundled HEAD, and a new tree is committed with parents (last HEAD unless the source already contains it, the
# source HEAD). A clean source that contains the last HEAD is bundled as is; an unchanged tree reuses the last HEAD.
lin=$HM_REPO/.dev/hb-lineage.git
[[ -d $lin ]] || git init -q --bare "$lin"
hb=$tmp/hb/hommy-browsers
mkdir -p "$tmp/hb"
git clone -q --no-hardlinks "$hb_src" "$hb"
git -C "$hb" remote set-url origin https://github.com/AlexDobrushskiy/hommy-browsers.git
src=$(git -C "$hb" rev-parse HEAD)
git -C "$hb_src" ls-files -z -co --exclude-standard | copy_listed "$hb_src" "$hb"
git -C "$hb_src" ls-files -z -d | while IFS= read -r -d '' f; do rm -f "$hb/$f"; done
git -C "$hb" add -A
tree=$(git -C "$hb" write-tree)
tip=''
if git -C "$lin" rev-parse -q --verify refs/heads/artifacts >/dev/null; then
  git -C "$hb" fetch -q "$lin" +refs/heads/artifacts:refs/lineage/tip
  tip=$(git -C "$hb" rev-parse refs/lineage/tip)
fi
tip_in_src=false
[[ -n $tip ]] && git -C "$hb" merge-base --is-ancestor "$tip" "$src" && tip_in_src=true
if [[ -n $tip ]] && [[ $(git -C "$hb" rev-parse "$tip^{tree}") == "$tree" ]] && git -C "$hb" merge-base --is-ancestor "$src" "$tip"; then
  head=$tip
elif [[ $tree == $(git -C "$hb" rev-parse "$src^{tree}") ]] && { [[ -z $tip ]] || $tip_in_src; }; then
  head=$src
else
  parents=()
  [[ -n $tip ]] && ! $tip_in_src && parents+=(-p "$tip")
  parents+=(-p "$src")
  when=$(git -C "$hb" log -1 --format=%cI "$src")
  head=$(GIT_AUTHOR_NAME=hommy-build GIT_AUTHOR_EMAIL=$noreply GIT_COMMITTER_NAME=hommy-build \
    GIT_COMMITTER_EMAIL=$noreply GIT_AUTHOR_DATE=$when GIT_COMMITTER_DATE=$when \
    git -C "$hb" commit-tree "$tree" "${parents[@]}" -m "work tree of ${src:0:7} (artifacts build, never pushed)")
fi
git -C "$hb" reset -q --hard "$head"
git -C "$hb" update-ref -d refs/lineage/tip 2>/dev/null || true
git -C "$lin" fetch -q "$hb" +HEAD:refs/heads/artifacts
COPYFILE_DISABLE=1 tar -czf "$new/hommy-browsers.tar.gz" -C "$tmp/hb" --exclude node_modules hommy-browsers
echo "hommy-browsers $(git -C "$hb" rev-parse --short HEAD): hommy-browsers.tar.gz"

# 3. This checkout, as a plain directory.
mkdir -p "$new/app"
git ls-files -z -co --exclude-standard | copy_listed "$HM_REPO" "$new/app"
echo "app: $(find "$new/app" -type f | wc -l | tr -d ' ') files"

# 4. The installer and its Brewfile (top level, so `./install.sh` runs from the bundle's parent too).
for f in install.sh Brewfile; do
  if [[ -f $f ]]; then cp -p "$f" "$new/$f"; else echo "warning: no $f in the checkout yet: not bundled" >&2; fi
done

# 5. The fakes.
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o "$new/fakes/hommy-e2e" ./cmd/hommy-e2e
echo "fakes: hommy-e2e"

rm -rf "$out"
mv "$new" "$out"
echo "artifacts: $out (version $version)"
