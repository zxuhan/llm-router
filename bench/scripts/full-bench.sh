#!/usr/bin/env bash
#
# Full cloud bench: matrix of {Qwen2.5-7B, Qwen2.5-14B} × {SESSIONS=4,8,12,16,24}.
# This is the one-button command that produces the README's headline charts.
#
# For each model in MODELS_TSV:
#   1. Auto-download (skipped if already present)
#   2. Run concurrency-sweep.sh (which itself runs cloud-vllm.sh at every
#      SESSIONS value, with fresh vLLM workers per (strategy, run, sessions))
#   3. Delete the model files to free container disk for the next model
#
# Each script along the way wipes its own OUT_DIR before writing, so a final
# `scp -r bench/results-sweep ./bench-results-sweep` always pulls a clean,
# non-overlapping snapshot. (rm -rf the local destination first to avoid the
# nested-results trap.)
#
# Disk budget per single model (only one is on disk at a time):
#   7B  -> ~14 GB
#   14B -> ~28 GB
#   32B -> ~64 GB (needs >= 100 GB container disk; commented out below)
#
# Usage (from repo root, on the pod):
#   bash bench/scripts/full-bench.sh
#
# Tunables (env):
#   MODELS_TSV     override the default model list (whitespace-separated:
#                  HF_REPO_ID  LOCAL_DIR)
#   SESSIONS_LIST  default "4 8 12 16 24" (passes through to concurrency-sweep.sh)
#   RUNS, TURNS, SYS_LEN, MAX_TOKENS, SATURATION_INFLIGHT  passthrough
#
# Output tree:
#   bench/results-sweep/qwen2.5-7b/sessions=N/   (one subdir per concurrency)
#   bench/results-sweep/qwen2.5-14b/sessions=N/
#
# Total wall time: ~3.5 hours on 4× A100 80GB SXM (7B sweep ~1.5h + 14B sweep ~2h).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

export PATH="/usr/local/go/bin:${PATH}"

# Edit this to add/remove models. Each line:  HF_REPO_ID  LOCAL_DIR
# The auto-download in cloud-vllm.sh expects LOCAL_DIR's basename to map
# to a known repo (see cloud-vllm.sh's derive_model_id), but you can also
# pass an explicit MODEL_ID via the env when you call full-bench.sh.
MODELS_TSV="${MODELS_TSV:-$(cat <<'EOF'
Qwen/Qwen2.5-7B-Instruct    models/qwen2.5-7b
Qwen/Qwen2.5-14B-Instruct   models/qwen2.5-14b
# Qwen/Qwen2.5-32B-Instruct models/qwen2.5-32b
EOF
)}"

SESSIONS_LIST="${SESSIONS_LIST:-4 8 12 16 24}"
SWEEP_OUT="bench/results-sweep"

# Wipe the entire sweep tree at the start of a full-bench so the final
# scp pulls only the data from THIS run.
rm -rf "$SWEEP_OUT"
mkdir -p "$SWEEP_OUT"

START_TIME=$(date +%s)
COMPLETED=()
FAILED=()

while IFS=$' \t' read -r model_id local_dir _rest; do
  [[ -z "${model_id:-}" || "${model_id:0:1}" == "#" ]] && continue

  short=$(basename "$local_dir")

  echo
  echo "################################################################################"
  echo "#  full-bench: $short  ($model_id)  sweeping SESSIONS=$SESSIONS_LIST"
  echo "################################################################################"

  if MODEL_ID="$model_id" \
       MODEL_DIR="$local_dir" \
       SESSIONS_LIST="$SESSIONS_LIST" \
       bash bench/scripts/concurrency-sweep.sh; then
    COMPLETED+=("$short")
  else
    echo "[full-bench] WARN: $short failed; recording and moving on" >&2
    FAILED+=("$short")
  fi

  # Free disk for the next model. (Only one model on disk at a time.)
  echo "[full-bench] freeing disk: rm -rf $local_dir"
  rm -rf "$local_dir"

done <<< "$MODELS_TSV"

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

ABS_OUT="$(cd "$SWEEP_OUT" && pwd)"
PUBLIC_IP="$(curl -s --max-time 3 https://api.ipify.org 2>/dev/null || echo '<pod-host>')"

cat <<EOF

################################################################################
  ✅  FULL-BENCH DONE  --  SAFE TO TERMINATE THE POD AFTER scp.
################################################################################

   wall time : ${ELAPSED}s  (~$((ELAPSED / 60)) min)
   completed : ${COMPLETED[*]}
EOF
if [ ${#FAILED[@]} -gt 0 ]; then
  echo "   FAILED    : ${FAILED[*]}"
fi
cat <<EOF
   sessions  : $SESSIONS_LIST
   results   : ${ABS_OUT}/

PRE-FLIGHT CHECKS (do all THREE before terminating):

   1. ls $SWEEP_OUT/*/sessions=*/real.md
      -> one per (model, concurrency) point; verify nothing missing.

   2. for d in $SWEEP_OUT/*/sessions=*/; do echo "=== \$d ==="; head -8 "\$d/real.md"; done
      -> sanity check the prefixaware row. KV cached should be > 0% now.

   3. SCP everything to your laptop. ALWAYS rm -rf the local destination first.
      RUN ON YOUR LAPTOP (not the pod):

         rm -rf ./bench-results-sweep
         scp -P <pod-ssh-port> -r root@${PUBLIC_IP}:${ABS_OUT} ./bench-results-sweep
         ls bench-results-sweep/*/sessions=*/real.md

ON YOUR LAPTOP, AFTER SCP:

   for short in qwen2.5-7b qwen2.5-14b; do
     [ -d "bench-results-sweep/\$short" ] && \\
       python3 bench/scripts/sweep-plot.py \\
         --input "bench-results-sweep/\$short" \\
         --out "docs/sweep-\${short}.png"
   done

   git add docs/sweep-*.png
   git commit -m "bench: cloud concurrency sweep at 7B and 14B"
   git push

THEN terminate the pod (RunPod dashboard -> your pod -> Terminate, NOT Stop).

################################################################################
EOF
