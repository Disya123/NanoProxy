#!/usr/bin/env bash
# Smoke test for nano-proxy. Hits the public proxy with three payloads:
#   1. non-stream chat completion
#   2. streamed chat completion (with stream_options.include_usage)
#   3. tool-calling chat completion (non-stream, expect has_tool_calls > 0)
#
# Requirements:
#   - a running nano-proxy (default http://127.0.0.1:8080)
#   - a client key created via the admin API (sknp_…)
#   - the real upstream NANOGPT_API_KEY configured in the proxy
#
# Usage:
#   CLIENT_KEY=sknp_… ./scripts/smoke.sh
#   PROXY=http://my.host:8080 ./scripts/smoke.sh
set -euo pipefail

PROXY="${PROXY:-http://127.0.0.1:8080}"
KEY="${CLIENT_KEY:-}"
ADMIN="${ADMIN:-http://127.0.0.1:8081}"
ADMIN_TOKEN_VAL="${ADMIN_TOKEN:-}"

if [[ -z "$KEY" ]]; then
  echo "CLIENT_KEY is required (export CLIENT_KEY=sknp_…)" >&2
  exit 2
fi

bold() { printf "\033[1m%s\033[0m\n" "$*"; }
ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
warn() { printf "  \033[33m!\033[0m %s\n" "$*"; }
fail() { printf "  \033[31m✗\033[0m %s\n" "$*"; exit 1; }

bold "1) non-stream chat completion"
NONSTREAM_BODY=$(curl -fsS "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "minimax/minimax-m2.7",
    "messages": [{"role":"user","content":"Reply with the single word: pong"}],
    "stream": false
  }')
echo "$NONSTREAM_BODY" | head -c 200; echo " …"
echo "$NONSTREAM_BODY" | grep -q '"usage"' && ok "usage block present" || warn "no usage block"
echo "$NONSTREAM_BODY" | grep -q '"x_nanogpt_pricing"' && ok "x_nanogpt_pricing present" || warn "no pricing field"
echo "$NONSTREAM_BODY" | grep -q '"content":"pong"' && ok "model returned pong" || warn "unexpected content"

bold "2) streamed chat completion (SSE)"
TMPF=$(mktemp)
curl -fsSN "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -D "$TMPF.headers" -o "$TMPF.body" \
  -d '{
    "model": "minimax/minimax-m2.7",
    "messages": [{"role":"user","content":"Count from 1 to 3"}],
    "stream": true,
    "stream_options": {"include_usage": true}
  }' || fail "stream request failed"
grep -qi '^content-type: text/event-stream' "$TMPF.headers" && ok "SSE content-type" || warn "no SSE content-type"
grep -q '"usage"' "$TMPF.body" && ok "stream delivered usage" || warn "no usage in stream"
grep -q '"x_nanogpt_pricing"' "$TMPF.body" && ok "stream delivered pricing" || warn "no pricing in stream"
grep -q '\[DONE\]' "$TMPF.body" && ok "stream terminated with [DONE]" || warn "no [DONE] terminator"
rm -f "$TMPF" "$TMPF.headers"

bold "3) tool-calling chat completion"
TOOL_BODY=$(curl -fsS "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "minimax/minimax-m2.7",
    "messages": [{"role":"user","content":"What is the weather in Reykjavík?"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Return the current weather for a city.",
        "parameters": {
          "type": "object",
          "properties": {"city": {"type": "string"}},
          "required": ["city"]
        }
      }
    }]
  }')
echo "$TOOL_BODY" | grep -q '"tool_calls"' && ok "tool_calls returned" || warn "no tool_calls"
echo "$TOOL_BODY" | grep -q '"finish_reason":"tool_calls"' && ok "finish_reason=tool_calls" || warn "finish_reason is not tool_calls"

if [[ -n "$ADMIN_TOKEN_VAL" ]]; then
  bold "4) admin API: most recent request"
  cookie_jar=$(mktemp)
  curl -fsS -c "$cookie_jar" -X POST "$ADMIN/admin/api/login" \
    -H "Content-Type: application/json" \
    -d "{\"token\":\"$ADMIN_TOKEN_VAL\"}" > /dev/null
  curl -fsS -b "$cookie_jar" "$ADMIN/admin/api/requests?limit=1" \
    | head -c 400 | sed 's/.*/  &/'
  echo
  ok "admin API authenticated"
  rm -f "$cookie_jar"
fi

bold "smoke test complete"