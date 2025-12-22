#!/usr/bin/env bash
#
# Cloud benchmark: boot N vLLM workers (one per visible GPU), run the
# multi-seed bench against them via our router, then write the report
# and a clear "DONE" banner so you know it's safe to scp results out and
# terminate the pod.
#
# Pre-reqs: bash scripts/install-cloud.sh (Go, vLLM, model already there).
#
# What it does:
#   1. For each GPU index, launch `vllm serve <MODEL_DIR>` on port 800{i}
#      with --enable-prefix-caching pinned via CUDA_VISIBLE_DEVICES.
#   2. Wait for every worker's /health to respond.
#   3. Run bench/scripts/real-llm.sh (the existing multi-seed orchestrator)
#      with WORKER_PORTS= matching what we just booted.
#   4. Tear down workers, print the SCP command and a SAFE-TO-TERMINATE banner.
#
# Tunables (env):
#   MODEL_DIR    default models/qwen2.5-7b
#   N_WORKERS    default = $(nvidia-smi -L | wc -l)
#   START_PORT   default 8001
#   GPU_MEM_UTIL default 0.90
#   MAX_MODEL_LEN default 8192
#   RUNS         default 3 (passed to bench/scripts/real-llm.sh)
#   SESSIONS     default 12
#   TURNS        default 4
#   SYS_LEN      default 2048
#   MAX_TOKENS   default 16
#   SEED         default 17
#   OUT_DIR      default bench/results

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

MODEL_DIR="${MODEL_DIR:-models/qwen2.5-7b}"
N_WORKERS="${N_WORKERS:-$(nvidia-smi -L 2>/dev/null | wc -l | tr -d ' ')}"
START_PORT="${START_PORT:-8001}"
GPU_MEM_UTIL="${GPU_MEM_UTIL:-0.90}"
MAX_MODEL_LEN="${MAX_MODEL_LEN:-8192}"

RUNS="${RUNS:-3}"
SESSIONS="${SESSIONS:-12}"
TURNS="${TURNS:-4}"
SYS_LEN="${SYS_LEN:-2048}"
MAX_TOKENS="${MAX_TOKENS:-16}"
SEED="${SEED:-17}"
OUT_DIR="${OUT_DIR:-bench/results}"

if [ ! -f "${MODEL_DIR}/config.json" ]; then
  echo "ERROR: model not found at ${MODEL_DIR}" >&2
  echo "Run scripts/install-cloud.sh first." >&2
  exit 1
fi

if [ "${N_WORKERS}" -lt 2 ]; then
  echo "ERROR: need at least 2 GPUs; nvidia-smi sees ${N_WORKERS}" >&2
  exit 1
fi

mkdir -p "${OUT_DIR}/raw" /tmp/vllm-logs
rm -rf "${OUT_DIR}"/raw* "${OUT_DIR}"/real-*.json "${OUT_DIR}"/real.md

log() { printf "\n[cloud-bench] %s\n" "$*"; }

# WORKER_PORTS string used by bench/scripts/real-llm.sh
ports=()
for i in $(seq 0 $((N_WORKERS - 1))); do
  ports+=( $((START_PORT + i)) )
done
WORKER_PORTS="${ports[*]}"
log "will run with N=${N_WORKERS} workers on ports: ${WORKER_PORTS}"

# vLLM bootstrap helper (used by real-llm.sh via the env hook below)
boot_vllm_workers() {
  local i=0
  for port in "${ports[@]}"; do
    CUDA_VISIBLE_DEVICES="$i" \
      python3 -m vllm.entrypoints.openai.api_server \
        --model "${MODEL_DIR}" \
        --port "${port}" \
        --enable-prefix-caching \
        --gpu-memory-utilization "${GPU_MEM_UTIL}" \
        --max-model-len "${MAX_MODEL_LEN}" \
        --disable-log-requests \
        > "/tmp/vllm-logs/w${i}.log" 2>&1 &
    echo $! > "/tmp/vllm-logs/w${i}.pid"
    i=$((i + 1))
  done
}

stop_vllm_workers() {
  for pidfile in /tmp/vllm-logs/w*.pid; do
    [ -f "$pidfile" ] || continue
    pid=$(cat "$pidfile" 2>/dev/null || true)
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 60); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.5
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
  done
}

wait_for_health() {
  for port in "${ports[@]}"; do
    log "waiting for worker on :${port} ..."
    for _ in $(seq 1 600); do
      if curl -fs "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then break; fi
      sleep 1
    done
    if ! curl -fs "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then
      echo "ERROR: worker on :${port} never became healthy. See /tmp/vllm-logs/" >&2
      tail -30 "/tmp/vllm-logs/w$((port - START_PORT)).log" >&2 || true
      stop_vllm_workers
      exit 1
    fi
  done
}

# real-llm.sh boots its own workers (llama-server). For vLLM we override
# its start_workers/stop_workers by exporting them BEFORE invoking it. The
# script reads them via `declare -F`, but the simplest approach: we just
# manage workers HERE and call cmd/bench directly with --real, mirroring
# what real-llm.sh does internally.

REAL=""
for port in "${ports[@]}"; do
  if [ -n "$REAL" ]; then REAL="$REAL,"; fi
  REAL="${REAL}http://127.0.0.1:${port}"
done

# ---- multi-seed bench ------------------------------------------------------
log "running ${RUNS} seeds x 4 strategies (fresh vLLM workers per (strategy, run))"
for strat in roundrobin random leastloaded prefixaware; do
  for run in $(seq 1 "${RUNS}"); do
    seed=$((SEED + run - 1))
    log "=== ${strat}  run=${run}  seed=${seed} ==="
    stop_vllm_workers
    sleep 2
    boot_vllm_workers
    wait_for_health

    suffix="-run${run}"
    rawdir="${OUT_DIR}/raw${suffix}"
    bin/bench \
      --real "${REAL}" \
      --strategy "${strat}" \
      --seed "${seed}" \
      --sessions "${SESSIONS}" \
      --turns "${TURNS}" \
      --system-len "${SYS_LEN}" \
      --max-tokens "${MAX_TOKENS}" \
      --json "${OUT_DIR}/real-${strat}${suffix}.json" \
      --raw-results "${rawdir}"
  done
done
stop_vllm_workers

# ---- aggregate -------------------------------------------------------------
log "aggregating multi-seed report"
go run ./bench/scripts/aggregate.go --multi \
  "${OUT_DIR}"/real-roundrobin-run*.json \
  "${OUT_DIR}"/real-random-run*.json \
  "${OUT_DIR}"/real-leastloaded-run*.json \
  "${OUT_DIR}"/real-prefixaware-run*.json \
  > "${OUT_DIR}/real.md"

# ---- pull-back instructions ------------------------------------------------
cat <<EOF

================================================================================
  ✅  DONE  --  bench finished cleanly. SAFE TO TERMINATE THE POD AFTER scp.
================================================================================

Headline report:   ${OUT_DIR}/real.md
Per-strategy JSON: ${OUT_DIR}/real-*.json
Per-request JSONL: ${OUT_DIR}/raw-run*/

To copy results back to your laptop, run THIS on YOUR LAPTOP (not on the pod):

  scp -P <pod-ssh-port> -r root@<pod-host>:${ROOT}/${OUT_DIR} ./bench-results-cloud

Then on your laptop, regenerate the figures + commit:

  python3 bench/scripts/hero.py --input bench-results-cloud --out docs/hero.png
  python3 bench/scripts/plot.py --input bench-results-cloud --out docs/cdf.png
  cp bench-results-cloud/real.md docs/results-cloud.md

Once results are safely on your laptop:
  - RunPod web dashboard -> your pod -> "Stop" or "Terminate"
  - or: runpodctl stop pod <pod-id>

================================================================================
EOF
