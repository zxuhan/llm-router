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
# WORKER_PORTS is a space-separated list of ports for the N workers.
# Default is two; set e.g. WORKER_PORTS="8001 8002 8003" for three.
WORKER_PORTS="${WORKER_PORTS:-8001 8002}"
SEED="${SEED:-42}"
SESSIONS="${SESSIONS:-8}"
TURNS="${TURNS:-4}"
SYS_LEN="${SYS_LEN:-512}"
CODE_LEN="${CODE_LEN:-0}"
MAX_TOKENS="${MAX_TOKENS:-16}"
CTX_SIZE="${CTX_SIZE:-4096}"
# RUNS controls multi-seed averaging. Each (strategy, run) combo gets its
# own fresh worker boot. Output JSONs are suffixed -run<N> so the
# aggregator can compute CIs across runs.
RUNS="${RUNS:-1}"
# SATURATION lets the orchestrator pass a non-default saturation_inflight
# to the prefix-aware strategy without editing the bench binary; useful
# for the safety-valve ablation in bench/scripts/ablate-saturation.sh.
SATURATION="${SATURATION:-}"

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
  echo "  booting fresh workers on ports: $WORKER_PORTS"
  i=0
  for port in $WORKER_PORTS; do
    llama-server -m "$MODEL" --port "$port" --ctx-size "$CTX_SIZE" --parallel 2 \
      --cache-reuse 256 --no-webui --log-disable >/tmp/llm-router-w$i.log 2>&1 &
    echo $! > /tmp/llm-router-w$i.pid
    i=$((i+1))
  done

  # Wait for every /health endpoint to come up.
  for port in $WORKER_PORTS; do
    for _ in $(seq 1 120); do
      if curl -s "http://127.0.0.1:$port/health" | grep -q '"ok"'; then break; fi
      sleep 0.5
    done
  done
}

stop_workers() {
  for pidfile in /tmp/llm-router-w*.pid; do
    [ -f "$pidfile" ] || continue
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
  done
}
trap stop_workers EXIT

REAL=""
for port in $WORKER_PORTS; do
  if [ -n "$REAL" ]; then REAL="$REAL,"; fi
  REAL="${REAL}http://127.0.0.1:$port"
done

for strat in roundrobin random leastloaded prefixaware; do
  for run in $(seq 1 "$RUNS"); do
    suffix=""
    rawdir="$OUT_DIR/raw"
    if [ "$RUNS" -gt 1 ]; then
      suffix="-run$run"
      rawdir="$OUT_DIR/raw$suffix"
    fi
    seed=$((SEED + run - 1))
    echo "=== $strat run=$run seed=$seed ==="
    stop_workers
    start_workers

    extra_args=""
    if [ -n "$SATURATION" ] && [ "$strat" = "prefixaware" ]; then
      extra_args="--saturation-inflight $SATURATION"
    fi

    # shellcheck disable=SC2086
    bin/bench \
      --real "$REAL" \
      --strategy "$strat" \
      --seed "$seed" \
      --sessions "$SESSIONS" \
      --turns "$TURNS" \
      --system-len "$SYS_LEN" \
      --code-context-len "$CODE_LEN" \
      --max-tokens "$MAX_TOKENS" \
      --json "$OUT_DIR/real-$strat$suffix.json" \
      --raw-results "$rawdir" \
      $extra_args
  done
done

stop_workers

# Aggregate. When RUNS=1 the layout matches the original single-run report.
# When RUNS>1 the aggregator computes mean and stddev across runs per
# strategy.
if [ "$RUNS" -eq 1 ]; then
  go run ./bench/scripts/aggregate.go \
    "$OUT_DIR"/real-roundrobin.json \
    "$OUT_DIR"/real-random.json \
    "$OUT_DIR"/real-leastloaded.json \
    "$OUT_DIR"/real-prefixaware.json \
    > "$OUT_DIR/real.md"
else
  go run ./bench/scripts/aggregate.go --multi \
    "$OUT_DIR"/real-roundrobin-run*.json \
    "$OUT_DIR"/real-random-run*.json \
    "$OUT_DIR"/real-leastloaded-run*.json \
    "$OUT_DIR"/real-prefixaware-run*.json \
    > "$OUT_DIR/real.md"
fi

echo
echo "wrote $OUT_DIR/real.md and $OUT_DIR/real-*.json"
