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
#   1. Preflight: nvidia-smi, vllm import, go on PATH all check out.
#   2. For each GPU index i, launch vLLM on port 800{i+1} with
#      --enable-prefix-caching, --served-model-name fake-model
#      (so the trace's "fake-model" requests are accepted), pinned via
#      CUDA_VISIBLE_DEVICES=i.
#   3. Wait for every worker's /health.
#   4. Warm each worker once with a unique throwaway prompt so the first
#      bench request isn't paying CUDA-kernel-JIT cost on top of cold
#      prefill (we want to measure cold *prefill*, not cold compile).
#   5. Run the multi-seed bench (3 seeds x 4 strategies, fresh workers
#      per (strategy, run) so KV caches start empty).
#   6. Tear down workers, write Markdown report, print the SCP command and
#      a SAFE-TO-TERMINATE banner.
#
# Tunables (env):
#   MODEL_DIR     default models/qwen2.5-7b
#   N_WORKERS     default = $(nvidia-smi -L | wc -l)
#   START_PORT    default 8001
#   GPU_MEM_UTIL  default 0.90
#   MAX_MODEL_LEN default 8192
#   RUNS          default 3
#   SESSIONS      default 12
#   TURNS         default 4
#   SYS_LEN       default 2048
#   MAX_TOKENS    default 16
#   SEED          default 17
#   OUT_DIR       default bench/results

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Make sure Go is on PATH even in a fresh non-login shell (install-cloud.sh
# wrote this to ~/.bashrc but the current shell may not have re-sourced).
export PATH="/usr/local/go/bin:${PATH}"

MODEL_DIR="${MODEL_DIR:-models/qwen2.5-7b}"
N_WORKERS="${N_WORKERS:-$(nvidia-smi -L 2>/dev/null | wc -l | tr -d ' ')}"
# Default to a high, unlikely-to-be-taken port range. RunPod's PyTorch image
# has nginx running on :8001 by default; a vLLM bind there fails silently
# and nginx happily 200s /health while 405-ing every POST /v1/chat/completions,
# producing an entire bench report of garbage (we hit this once, hence 18001).
START_PORT="${START_PORT:-18001}"
GPU_MEM_UTIL="${GPU_MEM_UTIL:-0.90}"
MAX_MODEL_LEN="${MAX_MODEL_LEN:-8192}"

RUNS="${RUNS:-3}"
SESSIONS="${SESSIONS:-12}"
TURNS="${TURNS:-4}"
SYS_LEN="${SYS_LEN:-2048}"
MAX_TOKENS="${MAX_TOKENS:-16}"
SEED="${SEED:-17}"
OUT_DIR="${OUT_DIR:-bench/results}"

# vLLM has to advertise the same model name the trace generator uses,
# otherwise it 404s every request.
SERVED_MODEL_NAME="fake-model"

log() { printf "\n[cloud-bench] %s\n" "$*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

# ---- preflight -------------------------------------------------------------
log "preflight"
command -v nvidia-smi >/dev/null || fail "nvidia-smi not found; is this a GPU pod?"
command -v go >/dev/null || fail "go not found on PATH; rerun scripts/install-cloud.sh"
python3 -c "import vllm" 2>/dev/null || fail "vllm not importable; rerun scripts/install-cloud.sh"
[ -f "${MODEL_DIR}/config.json" ] || fail "model not found at ${MODEL_DIR}; rerun scripts/install-cloud.sh"
[ -x bin/bench ] || fail "bin/bench not built; run: make build"
[ "${N_WORKERS}" -ge 2 ] || fail "need >= 2 GPUs; nvidia-smi sees ${N_WORKERS}"

# Verify none of the ports we're about to use are already taken by some other
# process on the host (nginx on RunPod, leftover vLLM, etc.). A silent vLLM
# bind failure produces invalid bench data, so this is a hard fail.
for i in $(seq 0 $((N_WORKERS - 1))); do
  port=$((START_PORT + i))
  if ss -tln "( sport = :${port} )" 2>/dev/null | grep -q LISTEN; then
    echo "ERROR: port ${port} is already in use; vLLM bind would fail silently." >&2
    echo "       Holder: $(ss -tlnp "( sport = :${port} )" 2>/dev/null | tail -1)" >&2
    echo "       Pick a different START_PORT, or stop the holder." >&2
    exit 1
  fi
done

mkdir -p "${OUT_DIR}/raw" /tmp/vllm-logs
rm -rf "${OUT_DIR}"/raw* "${OUT_DIR}"/real-*.json "${OUT_DIR}"/real.md

ports=()
for i in $(seq 0 $((N_WORKERS - 1))); do
  ports+=( $((START_PORT + i)) )
done
log "will run with N=${N_WORKERS} workers on ports: ${ports[*]}"

# ---- worker management -----------------------------------------------------
boot_vllm_workers() {
  local i=0
  for port in "${ports[@]}"; do
    CUDA_VISIBLE_DEVICES="$i" \
      python3 -m vllm.entrypoints.openai.api_server \
        --model "${MODEL_DIR}" \
        --served-model-name "${SERVED_MODEL_NAME}" \
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
  # Kill recorded PIDs first.
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
  # vLLM spawns helper processes; reap any orphans by name.
  pkill -9 -f "vllm.entrypoints.openai.api_server" 2>/dev/null || true
  # Free GPU memory before next boot.
  sleep 2
}
trap stop_vllm_workers EXIT

wait_for_health() {
  for port in "${ports[@]}"; do
    log "waiting for worker on :${port}"
    for _ in $(seq 1 600); do
      if curl -fs "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then break; fi
      sleep 1
    done
    if ! curl -fs "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then
      echo "worker on :${port} never became healthy. Last 30 log lines:" >&2
      tail -30 "/tmp/vllm-logs/w$((port - START_PORT)).log" >&2 || true
      exit 1
    fi
  done
}

# Pre-warm CUDA kernels on each worker with a UNIQUE prompt per worker, AND
# verify each worker actually answers POST /v1/chat/completions with 200.
# /health returning OK is not enough: a misconfigured vLLM (wrong
# --served-model-name, missing model file, FastAPI route shadowing) can
# pass /health while rejecting every chat request with 405. We hit that bug
# in production and produced an entire benchmark report where one dead
# worker masqueraded as "PA p50 282µs". Hard-fail here instead.
warmup_workers() {
  local i=0
  local seed_phrase status
  for port in "${ports[@]}"; do
    seed_phrase="warmup-w${i}-$(date +%s%N)"
    status=$(curl -s -o /dev/null -w '%{http_code}' \
      -X POST "http://127.0.0.1:${port}/v1/chat/completions" \
      -H 'Content-Type: application/json' \
      -d "{\"model\":\"${SERVED_MODEL_NAME}\",\"messages\":[{\"role\":\"user\",\"content\":\"${seed_phrase}\"}],\"max_tokens\":1}" \
      || echo "000")
    if [ "${status}" != "200" ]; then
      echo "ERROR: worker w${i} on :${port} returned HTTP ${status} to warmup POST." >&2
      echo "       Last 30 log lines:" >&2
      tail -30 "/tmp/vllm-logs/w${i}.log" >&2 || true
      echo "       Hint: check --served-model-name matches the trace generator's 'fake-model'." >&2
      exit 1
    fi
    i=$((i + 1))
  done
}

REAL=""
for port in "${ports[@]}"; do
  if [ -n "$REAL" ]; then REAL="$REAL,"; fi
  REAL="${REAL}http://127.0.0.1:${port}"
done

# ---- multi-seed bench ------------------------------------------------------
log "running ${RUNS} seeds x 4 strategies (fresh vLLM workers per (strategy, run))"
START_TIME=$(date +%s)
for strat in roundrobin random leastloaded prefixaware; do
  for run in $(seq 1 "${RUNS}"); do
    seed=$((SEED + run - 1))
    log "=== ${strat}  run=${run}  seed=${seed} ==="
    stop_vllm_workers
    boot_vllm_workers
    wait_for_health
    warmup_workers

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
END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

# ---- aggregate -------------------------------------------------------------
log "aggregating multi-seed report"
go run ./bench/scripts/aggregate.go --multi \
  "${OUT_DIR}"/real-roundrobin-run*.json \
  "${OUT_DIR}"/real-random-run*.json \
  "${OUT_DIR}"/real-leastloaded-run*.json \
  "${OUT_DIR}"/real-prefixaware-run*.json \
  > "${OUT_DIR}/real.md"

# ---- pull-back instructions ------------------------------------------------
ABS_OUT="$(cd "${OUT_DIR}" && pwd)"
PUBLIC_IP="$(curl -s --max-time 3 https://api.ipify.org 2>/dev/null || echo '<pod-host>')"

cat <<EOF

================================================================================
  ✅  DONE  --  bench finished cleanly. SAFE TO TERMINATE THE POD AFTER scp.
================================================================================

   wall time : ${ELAPSED}s
   workers   : ${N_WORKERS} vLLM × ${SERVED_MODEL_NAME} on ${MODEL_DIR}
   trace     : ${RUNS} runs × ${SESSIONS} sessions × ${TURNS} turns = $((RUNS * SESSIONS * TURNS * 4)) total requests across 4 strategies
   results   : ${ABS_OUT}/

PRE-FLIGHT CHECKS (do all THREE before terminating):

   1. cat ${OUT_DIR}/real.md | head -20
      -> table must show all 4 strategies; PA's KV-cached number should
         be the highest in the column.

   2. ls ${OUT_DIR}/raw-run*/prefixaware.jsonl
      -> 3 files (one per seed), each with 18+ JSONL lines.

   3. SCP the directory to your laptop and verify it arrived:
      RUN ON YOUR LAPTOP (not the pod):

         scp -P <pod-ssh-port> -r root@${PUBLIC_IP}:${ABS_OUT} ./bench-results-cloud
         ls bench-results-cloud/real.md   # must exist locally

ON YOUR LAPTOP, AFTER SCP:

   python3 bench/scripts/hero.py  --input bench-results-cloud --out docs/hero-cloud.png
   python3 bench/scripts/plot.py  --input bench-results-cloud --out docs/cdf-cloud.png
   cp bench-results-cloud/real.md docs/results-cloud.md

   git add docs/hero-cloud.png docs/cdf-cloud.png docs/results-cloud.md
   git commit -m "bench: cloud results on ${N_WORKERS}× GPU + vLLM + ${SERVED_MODEL_NAME}"
   git push

THEN terminate the pod:

   - RunPod web dashboard -> your pod -> Terminate (NOT 'Stop'; Stop keeps disk billed)
   - or: runpodctl stop pod <pod-id>

================================================================================
EOF
