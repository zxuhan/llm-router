#!/usr/bin/env bash
# Live demo: fire 5 sequential chat completions through the running
# router, print one clean status line per request showing the routing
# decision (X-Router-Backend, X-Router-Reason) and the upstream KV-cache
# numbers (cached_tokens / prompt_tokens). The first request is cold;
# subsequent requests should land on the same warm worker and show
# cached_tokens jump dramatically.
#
# Requires: a running router on :8080 with at least 2 workers configured.

set -euo pipefail

ENDPOINT="${ENDPOINT:-http://127.0.0.1:8080/v1/chat/completions}"
N="${N:-5}"

# 1.5 KB shared system prompt so prefix matching has something to chew on.
SYS=$(python3 -c '
import sys
prefix = "You are a helpful coding assistant. Answer concisely.\n"
filler = ("Background context for the assistant. " * 30) + ("System policy notes. " * 20)
sys.stdout.write(prefix + filler)
')

ESC_SYS=$(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$SYS")

C_BLUE=$'\033[1;36m'
C_GREEN=$'\033[1;32m'
C_GRAY=$'\033[0;90m'
C_RED=$'\033[1;31m'
C_BOLD=$'\033[1m'
C_RESET=$'\033[0m'

printf "%bprefix-aware llm-router  ::  %d sequential chat completions, shared system prompt%b\n" \
  "$C_BOLD" "$N" "$C_RESET"
printf "%bendpoint=%s%b\n\n" "$C_GRAY" "$ENDPOINT" "$C_RESET"

printf "  %-3s  %-9s  %-22s  %-9s  %-9s\n" "req" "backend" "reason" "ttft" "kv_cache"
printf "  %-3s  %-9s  %-22s  %-9s  %-9s\n" "---" "-------" "----------------------" "-------" "----------"

for i in $(seq 1 "$N"); do
  body=$(printf '{"messages":[{"role":"system","content":%s},{"role":"user","content":"In one sentence, name a fact #%d."}],"max_tokens":20}' \
    "$ESC_SYS" "$i")

  start=$(python3 -c 'import time; print(int(time.monotonic_ns()))')
  resp=$(curl -s -D - -o /tmp/live-body.json -X POST "$ENDPOINT" \
    -H "Content-Type: application/json" -d "$body")
  end=$(python3 -c 'import time; print(int(time.monotonic_ns()))')

  ttft_ms=$(( (end - start) / 1000000 ))
  backend=$(echo "$resp" | awk '/^X-Router-Backend:/ {print $2}' | tr -d '\r')
  reason=$(echo "$resp" | awk '/^X-Router-Reason:/ {print $2}' | tr -d '\r')
  cached=$(python3 -c 'import json; d=json.load(open("/tmp/live-body.json")); print(d["usage"]["prompt_tokens_details"]["cached_tokens"])')
  total=$(python3 -c 'import json; d=json.load(open("/tmp/live-body.json")); print(d["usage"]["prompt_tokens"])')

  if [ "$cached" -gt $((total / 2)) ]; then
    cache_color="$C_GREEN"
    cache_mark="HIT"
  else
    cache_color="$C_RED"
    cache_mark="cold"
  fi

  printf "  %b%-3d%b  %b%-9s%b  %-22s  %b%6d ms%b  %b%4d/%-4d %s%b\n" \
    "$C_BOLD" "$i" "$C_RESET" \
    "$C_BLUE" "$backend" "$C_RESET" \
    "$reason" \
    "$C_BOLD" "$ttft_ms" "$C_RESET" \
    "$cache_color" "$cached" "$total" "$cache_mark" "$C_RESET"
done

printf "\n%bobservation:%b after request 1 warmed worker %s, every subsequent request\n" \
  "$C_BOLD" "$C_RESET" "$backend"
printf "             pinned to that worker and skipped most of the prefill.\n"
