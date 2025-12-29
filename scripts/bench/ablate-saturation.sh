#!/usr/bin/env bash
#
# Safety-valve threshold ablation. Runs prefix-aware four times against the
# same trace, varying SaturationInflight: 1 (extreme spilling), 4, 8 (default),
# 9999 (effectively disabled). Each run gets fresh workers so no KV-cache
# carryover.
#
# Output:
#   bench/results/ablation/abl-sat-<sat>.json
#   bench/results/ablation/abl-sat.md
#
# Usage:
#   bash scripts/bench/ablate-saturation.sh
#
# Requires the same llama-server install + model as scripts/bench/real-llm.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

MODEL="${MODEL:-models/qwen2.5-1.5b.gguf}"
WORKER_PORTS="${WORKER_PORTS:-8001 8002 8003}"
SEED="${SEED:-17}"
SESSIONS="${SESSIONS:-12}"
TURNS="${TURNS:-3}"
SYS_LEN="${SYS_LEN:-2048}"
MAX_TOKENS="${MAX_TOKENS:-8}"
CTX_SIZE="${CTX_SIZE:-4096}"
SAT_VALUES="${SAT_VALUES:-1 4 8 9999}"

OUT_DIR="bench/results/ablation"
mkdir -p "$OUT_DIR"

if [ ! -f "$MODEL" ]; then
  echo "missing model file at $MODEL" >&2
  exit 1
fi
if [ ! -x bin/bench ]; then
  make bench-bin
fi

start_workers() {
  i=0
  for port in $WORKER_PORTS; do
    llama-server -m "$MODEL" --port "$port" --ctx-size "$CTX_SIZE" --parallel 2 \
      --cache-reuse 256 --no-webui --log-disable >/tmp/abl-w$i.log 2>&1 &
    echo $! > /tmp/abl-w$i.pid
    i=$((i+1))
  done
  for port in $WORKER_PORTS; do
    for _ in $(seq 1 120); do
      if curl -s "http://127.0.0.1:$port/health" | grep -q '"ok"'; then break; fi
      sleep 0.5
    done
  done
}

stop_workers() {
  for pidfile in /tmp/abl-w*.pid; do
    [ -f "$pidfile" ] || continue
    pid=$(cat "$pidfile" 2>/dev/null || true)
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 20); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
  done
}
trap stop_workers EXIT

REAL=""
for port in $WORKER_PORTS; do
  if [ -n "$REAL" ]; then REAL="$REAL,"; fi
  REAL="${REAL}http://127.0.0.1:$port"
done

for sat in $SAT_VALUES; do
  echo "=== sat=$sat ==="
  stop_workers
  start_workers
  bin/bench \
    --real "$REAL" \
    --strategy prefixaware \
    --saturation-inflight "$sat" \
    --seed "$SEED" \
    --sessions "$SESSIONS" \
    --turns "$TURNS" \
    --system-len "$SYS_LEN" \
    --max-tokens "$MAX_TOKENS" \
    --json "$OUT_DIR/abl-sat-$sat.json"
done

stop_workers

# Render a tiny ablation table inline (no extra Go program needed).
{
  echo "# Safety-valve threshold ablation"
  echo
  echo "Same trace ($SESSIONS sessions x $TURNS turns, ~${SYS_LEN}-char shared system prompt) replayed with prefix-aware at four saturation thresholds. Fresh workers each run."
  echo
  echo "| saturation_inflight | Hit rate | Pinned | Spilled | Fallback | KV cached | TTFT p50 | TTFT p95 | RPS |"
  echo "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
  for sat in $SAT_VALUES; do
    f="$OUT_DIR/abl-sat-$sat.json"
    [ -f "$f" ] || continue
    python3 - "$f" "$sat" <<'PY'
import json, sys
sat, path = sys.argv[2], sys.argv[1]
with open(path) as fp:
    d = json.load(fp)
s = d["summaries"][0]
def ms(ns): return f"{ns/1e6:.0f}ms" if ns < 1e9 else f"{ns/1e9:.2f}s"
print(f"| {sat} | {s['hit_rate']*100:.2f}% | {s['pinned_requests']} | {s['spilled_requests']} | {s['fallback_requests']} | {s['cache_token_rate']*100:.2f}% | {ms(s['ttft']['p50'])} | {ms(s['ttft']['p95'])} | {s['throughput_rps']:.2f} |")
PY
  done
  echo
  echo "Reading the table:"
  echo
  echo "- saturation_inflight=1 forces PA to spill aggressively; expect Spilled / Fallback to dominate over Pinned, hit rate to drop, and load to spread."
  echo "- saturation_inflight=9999 effectively disables the safety valve; PA pins everything, hit rate maxes out, but a single hot worker bottlenecks throughput under load."
  echo "- The default (8) is the project's recommended setpoint and the value used in the headline run."
} > "$OUT_DIR/abl-sat.md"

echo
echo "wrote $OUT_DIR/abl-sat.md and $OUT_DIR/abl-sat-*.json"
