#!/bin/bash
# Appends the pids of any running SecurityAgent (macOS's keychain dialog process) to FILE, five times a second.
set -u
while :; do
  /usr/bin/pgrep -x SecurityAgent >>"$1"
  sleep 0.2
done
