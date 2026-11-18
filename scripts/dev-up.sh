#!/bin/bash
# Brings up this checkout's slot (scripts/test-env.sh up); --restart takes it down first.
set -euo pipefail
here=$(dirname "$0")
if [[ ${1:-} == --restart ]]; then
  shift
  "$here/test-env.sh" down "$@" >/dev/null
fi
exec "$here/test-env.sh" up "$@"
