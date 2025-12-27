#!/usr/bin/env bash
#
# Concurrency sweep: run cloud-vllm.sh at multiple SESSIONS values to
# produce the killer chart -- "TTFT vs concurrency for each strategy".
#
# This is the demonstration that PA's value depends on the operating point:
# - Low concurrency: PA wins decisively (cache benefit > queuing penalty)
# - High concurrency: PA's safety valve kicks in, ties RR (well-tuned),
#                     or loses badly (poorly-tuned)
#
# Usage (from repo root, on the pod):
#   bash bench/scripts/concurrency-sweep.sh
#
# Output:
#   bench/results-sweep/<MODEL_SHORT>/sessions=N/
#       (one full result-dir per (model, sessions) pair)
#
# Tunables (env):
#   MODEL_DIR           default models/qwen2.5-7b
#   SESSIONS_LIST       default "4 8 12 16 24"
#   RUNS                default 3 (multi-seed at each point for error bars)
#   Other vars (TURNS, SYS_LEN, MAX_TOKENS, SATURATION_INFLIGHT, ...) flow
#   through to cloud-vllm.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

export PATH="/usr/local/go/bin:${PATH}"

MODEL_DIR="${MODEL_DIR:-models/qwen2.5-7b}"
SESSIONS_LIST="${SESSIONS_LIST:-4 8 12 16 24}"
SWEEP_OUT="bench/results-sweep"

short=$(basename "$MODEL_DIR")  # e.g. qwen2.5-7b

# Wipe this model's previous sweep tree (if any) so an scp -r afterwards
# pulls a clean dataset. cloud-vllm.sh wipes its OUT_DIR per point; this
# wipes the parent.
rm -rf "$SWEEP_OUT/$short"
mkdir -p "$SWEEP_OUT/$short"

START_TIME=$(date +%s)
COMPLETED=()
FAILED=()

for sessions in $SESSIONS_LIST; do
  out_dir="$SWEEP_OUT/$short/sessions=${sessions}"

  echo
  echo "================================================================================"
  echo "  sweep: $short  sessions=$sessions"
  echo "================================================================================"

  # Run with this concurrency. cloud-vllm.sh's own "DONE" banner will print
  # at the end of this point; ignore it -- only this script's final banner
  # is authoritative.
  if SESSIONS="$sessions" \
       MODEL_DIR="$MODEL_DIR" \
       OUT_DIR="$out_dir" \
       bash bench/scripts/cloud-vllm.sh; then
    COMPLETED+=("sessions=$sessions")
  else
    echo "[sweep] WARN: sessions=$sessions failed; recording and moving on" >&2
    FAILED+=("sessions=$sessions")
  fi
done

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

ABS_OUT="$(cd "$SWEEP_OUT" && pwd)"
PUBLIC_IP="$(curl -s --max-time 3 https://api.ipify.org 2>/dev/null || echo '<pod-host>')"

cat <<EOF

================================================================================
  ✅  CONCURRENCY-SWEEP DONE  --  SAFE TO TERMINATE THE POD AFTER scp.
================================================================================

   model     : $short
   sessions  : $SESSIONS_LIST
   wall time : ${ELAPSED}s  (~$((ELAPSED / 60)) min)
   completed : ${COMPLETED[*]}
EOF

if [ ${#FAILED[@]} -gt 0 ]; then
  echo "   FAILED    : ${FAILED[*]}"
fi

cat <<EOF
   results   : ${ABS_OUT}/$short/

PRE-FLIGHT CHECKS (do all THREE before terminating):

   1. ls $SWEEP_OUT/$short/sessions=*/real.md
      -> one per concurrency point.

   2. for d in $SWEEP_OUT/$short/sessions=*/; do
        echo "=== \$d ==="; head -10 "\$d/real.md"
      done
      -> sanity check the prefixaware row at each concurrency point.

   3. SCP everything to your laptop and verify it arrived. ALWAYS rm -rf
      the local destination FIRST.
      RUN ON YOUR LAPTOP (not the pod):

         rm -rf ./bench-results-sweep
         scp -P <pod-ssh-port> -r root@${PUBLIC_IP}:${ABS_OUT} ./bench-results-sweep
         ls bench-results-sweep/$short/sessions=*/real.md   # one per concurrency

ON YOUR LAPTOP, AFTER SCP:

   python3 bench/scripts/sweep-plot.py \\
       --input bench-results-sweep/$short \\
       --out docs/sweep-${short}.png

   git add docs/sweep-${short}.png
   git commit -m "bench: concurrency sweep for $short"
   git push

THEN terminate the pod (or run another sweep at a different MODEL_DIR first).

================================================================================
EOF
