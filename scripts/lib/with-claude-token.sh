#!/bin/bash
# Runs CMD with CLAUDE_CODE_OAUTH_TOKEN read from the current HOME's keychain (under sx: the scratch keychain), so the
# token is in that process's environment only: never in argv, a file or the output. Used by claude-session.sh.
set -euo pipefail
(($#)) || { echo "usage: with-claude-token.sh CMD [ARGS…]" >&2; exit 2; }
tok=$(/usr/bin/security find-generic-password -s hommy-test-claude-token -a hommy -w 2>/dev/null) || {
  echo "claude-session: no hommy-test-claude-token in the scratch keychain (scripts/test-env.sh up; ../ops/claude-token.sh import)" >&2
  exit 3
}
export CLAUDE_CODE_OAUTH_TOKEN=$tok
unset tok
exec "$@"
