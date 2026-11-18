#!/bin/bash
# One MCP tool call over streamable HTTP on 127.0.0.1:PORT (initialize, notifications/initialized, tools/call).
# Prints the call's result JSON; exit 1 on a JSON-RPC error or a result with isError.
#   scripts/mcp-call.sh PORT TOOL '<arguments JSON>'      (MCP_PATH overrides the endpoint path, default /mcp)
set -euo pipefail
(($# == 3)) || { sed -n '2,4s/^# \{0,1\}//p' "$0" >&2; exit 2; }
port=$1 tool=$2 args=$3
jq -e 'type == "object"' >/dev/null <<<"$args" 2>/dev/null || { echo "mcp-call: arguments must be a JSON object" >&2; exit 2; }
url=http://127.0.0.1:$port${MCP_PATH:-/mcp}
hdrs=$(mktemp)
trap 'rm -f "$hdrs"' EXIT

# rpc BODY: POST one message; prints the JSON-RPC response (from JSON or an SSE stream).
rpc() {
  local extra=() out
  [[ -n ${sid:-} ]] && extra=(-H "Mcp-Session-Id: $sid")
  out=$(curl -sS -m 120 -D "$hdrs" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2025-06-18' ${extra[@]+"${extra[@]}"} -d "$1" "$url") || { echo "mcp-call: no answer at $url" >&2; exit 1; }
  if grep -qi '^content-type: *text/event-stream' "$hdrs"; then
    sed -n 's/^data: *//p' <<<"$out" | jq -c 'select(.id != null)' | tail -1
  else
    printf '%s\n' "$out"
  fi
}

sid=''
init=$(rpc '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"hommy-mcp-call","version":"1"}}}')
jq -e '.result' >/dev/null <<<"$init" 2>/dev/null || { echo "mcp-call: initialize failed: $init" >&2; exit 1; }
sid=$(sed -n 's/^[Mm][Cc][Pp]-[Ss]ession-[Ii]d: *\([^[:space:]]*\).*/\1/p' "$hdrs" | head -1)
rpc '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
body=$(jq -cn --arg t "$tool" --argjson a "$args" '{jsonrpc: "2.0", id: 2, method: "tools/call", params: {name: $t, arguments: $a}}')
res=$(rpc "$body")
if ! jq -e '.result' >/dev/null <<<"$res" 2>/dev/null; then
  echo "mcp-call: $tool failed: $res" >&2
  exit 1
fi
jq -c '.result' <<<"$res"
jq -e '.result.isError != true' >/dev/null <<<"$res"
