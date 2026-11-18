#!/bin/bash
# A local GoReleaser snapshot (master spec §8.3) into dist/: the darwin/arm64 archive, checksums.txt and the cask as
# dist/hommy.rb. Nothing is published: no release, no tap push, no GITHUB_TOKEN. Works on a dirty tree.
#   scripts/release-snapshot.sh            version <next patch>-dev (from the latest v* tag; 0.0.1-dev without one)
#   HOMMY_VERSION=0.1.1-dev scripts/release-snapshot.sh
# Prints `version: V` and `archive: PATH` last.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
cd "$ROOT"
scripts/tools.sh ensure goreleaser >/dev/null
gr=$(scripts/tools.sh path goreleaser)

# The version base is the latest v* tag (the phase-NN tags are not versions); v0.0.0 without one.
tag=$(git describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || echo v0.0.0)

# No token is ever needed: keep any from the caller's environment away from GoReleaser.
env -u GITHUB_TOKEN -u GITLAB_TOKEN -u GITEA_TOKEN GORELEASER_CURRENT_TAG="$tag" "$gr" release --snapshot --clean \
  --skip=publish,announce,validate,sign,sbom,docker

cask=$(find dist -name hommy.rb -not -path dist/hommy.rb -print -quit)
[[ -n $cask ]] || { echo "release-snapshot: GoReleaser wrote no hommy.rb under dist/" >&2; exit 1; }
cp "$cask" dist/hommy.rb

version=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' dist/metadata.json)
archive=dist/hommy_${version}_darwin_arm64.tar.gz
[[ -f $archive ]] || { echo "release-snapshot: missing $archive" >&2; exit 1; }
echo "version: $version"
echo "archive: $ROOT/$archive"
