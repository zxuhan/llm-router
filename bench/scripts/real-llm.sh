#!/usr/bin/env bash
#
# Real-LLM benchmark: spin up two llama-server instances against a small GGUF
# model, then run each routing strategy with FRESH workers (so KV caches start
# empty for each run). Aggregate JSON summaries into one Markdown report.
#
# Requirements:
#   - llama-server on PATH (`brew install llama.cpp`)
#   - models/<MODEL>.gguf present (downloaded once; gitignored)
#   - the bench binary built (`make bench-bin`)
#
# Usage:
#   bash bench/scripts/real-llm.sh
#
# Outputs:
#   bench/results/real-<strategy>.json     (per-strategy JSON summary)
#   bench/results/real.md                  (aggregated Markdown)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

MODEL="${MODEL:-models/qwen2.5-0.5b.gguf}"
PORT_W0="${PORT_W0:-8001}"
PORT_W1="${PORT_W1:-8002}"
SEED="${SEED:-42}"
SESSIONS="${SESSIONS:-8}"
TURNS="${TURNS:-4}"
SYS_LEN="${SYS_LEN:-512}"
CODE_LEN="${CODE_LEN:-0}"
MAX_TOKENS="${MAX_TOKENS:-16}"

OUT_DIR="bench/results"
mkdir -p "$OUT_DIR"

if [ ! -f "$MODEL" ]; then
  echo "missing model file at $MODEL" >&2
  echo "fetch one with e.g.:" >&2
  echo "  curl -L -o $MODEL https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf" >&2
  exit 1
fi

if [ ! -x bin/bench ]; then
  make bench-bin
fi

start_workers() {
  echo "  booting fresh workers on ports $PORT_W0 / $PORT_W1..."
  llama-server -m "$MODEL" --port "$PORT_W0" --ctx-size 4096 --parallel 2 \
    --cache-reuse 256 --no-webui --log-disable >/tmp/llm-router-w0.log 2>&1 &
  echo $! > /tmp/llm-router-w0.pid
  llama-server -m "$MODEL" --port "$PORT_W1" --ctx-size 4096 --parallel 2 \
    --cache-reuse 256 --no-webui --log-disable >/tmp/llm-router-w1.log 2>&1 &
  echo $! > /tmp/llm-router-w1.pid

  # Wait for both /health endpoints to come up.
  for url in "http://127.0.0.1:$PORT_W0/health" "http://127.0.0.1:$PORT_W1/health"; do
    for _ in $(seq 1 60); do
      if curl -s "$url" | grep -q '"ok"'; then break; fi
      sleep 0.5
    done
  done
}

stop_workers() {
  for pidfile in /tmp/llm-router-w0.pid /tmp/llm-router-w1.pid; do
    if [ -f "$pidfile" ]; then
      pid=$(cat "$pidfile" 2>/dev/null || true)
      if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        # Wait briefly for graceful shutdown.
        for _ in $(seq 1 20); do
          if ! kill -0 "$pid" 2>/dev/null; then break; fi
          sleep 0.1
        done
        kill -9 "$pid" 2>/dev/null || true
      fi
      rm -f "$pidfile"
    fi
  done
}
trap stop_workers EXIT

REAL="http://127.0.0.1:$PORT_W0,http://127.0.0.1:$PORT_W1"

for strat in roundrobin random leastloaded prefixaware; do
  echo "=== $strat ==="
  stop_workers
  start_workers

  bin/bench \
    --real "$REAL" \
    --strategy "$strat" \
    --seed "$SEED" \
    --sessions "$SESSIONS" \
    --turns "$TURNS" \
    --system-len "$SYS_LEN" \
    --code-context-len "$CODE_LEN" \
    --max-tokens "$MAX_TOKENS" \
    --json "$OUT_DIR/real-$strat.json"
done

stop_workers

# Merge the four single-strategy JSONs into one Markdown using a tiny inline Go.
go run ./bench/scripts/aggregate.go \
  "$OUT_DIR"/real-roundrobin.json \
  "$OUT_DIR"/real-random.json \
  "$OUT_DIR"/real-leastloaded.json \
  "$OUT_DIR"/real-prefixaware.json \
  > "$OUT_DIR/real.md"

echo
echo "wrote $OUT_DIR/real.md and $OUT_DIR/real-*.json"
