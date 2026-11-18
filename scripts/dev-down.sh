#!/bin/bash
# Takes this checkout's slot down (scripts/test-env.sh down; --purge also removes its scratch root).
set -euo pipefail
exec "$(dirname "$0")/test-env.sh" down "$@"
