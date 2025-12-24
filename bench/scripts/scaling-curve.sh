#!/usr/bin/env bash
#
# Scaling-curve runner: run cloud-vllm.sh against multiple model sizes
# back-to-back to produce a "TTFT-savings vs model-size" curve.
#
# For each model in MODELS_TSV below:
#   1. Download (skipped if already present in MODEL_DIR)
#   2. Run the full multi-seed bench (cloud-vllm.sh handles vLLM boot/teardown,
#      fresh workers per (strategy,seed), aggregation)
#   3. Delete the model files to free container disk for the next iteration
#
# Disk budget (each model lives alone on disk thanks to delete-between):
#   - Up to 14B fits in a 50 GB container disk (the default RunPod recommend)
#   - 32B needs >= 100 GB container disk
#   - 70B+ needs tensor-parallel which defeats the routing demo; skip
#
# Usage (from repo root, on the pod):
#   bash bench/scripts/scaling-curve.sh
#
# Output:
#   bench/results-scaling/<short-name>/  (one subdir per model size)
#
# Edit MODELS_TSV to add/remove model sizes. The 32B line is commented out;
# uncomment if you're on a pod with >= 100 GB container disk.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Make sure Go is on PATH even in a fresh non-login shell.
export PATH="/usr/local/go/bin:${PATH}"

# Models to sweep, one per line. Whitespace-separated:
#   HF_REPO_ID    LOCAL_DIR    MAX_MODEL_LEN
# Lines starting with '#' are skipped.
MODELS_TSV=$(cat <<'EOF'
Qwen/Qwen2.5-7B-Instruct    models/qwen2.5-7b    8192
Qwen/Qwen2.5-14B-Instruct   models/qwen2.5-14b   8192
# Qwen/Qwen2.5-32B-Instruct models/qwen2.5-32b   4096
EOF
)

SCALING_OUT="bench/results-scaling"
mkdir -p "$SCALING_OUT"

START_TIME=$(date +%s)
COMPLETED=()

# Iterate through MODELS_TSV. We use here-string so the loop runs in the
# parent shell (so COMPLETED accumulates).
while IFS=$' \t' read -r model_id local_dir max_len _rest; do
  # Skip blanks and comments.
  [[ -z "${model_id:-}" || "${model_id:0:1}" == "#" ]] && continue

  short=$(basename "$local_dir")
  out_dir="$SCALING_OUT/$short"

  echo
  echo "================================================================================"
  echo "  scaling-curve: $short   ($model_id, max_model_len=$max_len)"
  echo "================================================================================"

  # 1. Download (idempotent: skip if config.json is already present).
  if [ -f "$local_dir/config.json" ]; then
    echo "[scaling] $short already on disk; skipping download"
  else
    echo "[scaling] downloading $model_id -> $local_dir"
    mkdir -p "$local_dir"
    HF_HUB_ENABLE_HF_TRANSFER=1 huggingface-cli download "$model_id" \
      --local-dir "$local_dir" \
      --local-dir-use-symlinks False
  fi

  # 2. Run the multi-seed bench. cloud-vllm.sh's "SAFE TO TERMINATE" banner
  #    will print at the end of THIS iteration; ignore it -- only this script's
  #    own final banner is authoritative.
  MODEL_DIR="$local_dir" \
    MAX_MODEL_LEN="$max_len" \
    OUT_DIR="$out_dir" \
    bash bench/scripts/cloud-vllm.sh

  # 3. Free the disk for the next model.
  echo "[scaling] freeing disk: rm -rf $local_dir"
  rm -rf "$local_dir"

  COMPLETED+=("$short")
done <<< "$MODELS_TSV"

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

ABS_OUT="$(cd "$SCALING_OUT" && pwd)"
PUBLIC_IP="$(curl -s --max-time 3 https://api.ipify.org 2>/dev/null || echo '<pod-host>')"

cat <<EOF

================================================================================
  ✅  SCALING-CURVE DONE  --  SAFE TO TERMINATE THE POD AFTER scp.
================================================================================

   wall time : ${ELAPSED}s  (~$((ELAPSED / 60)) min)
   completed : ${COMPLETED[*]}
   results   : ${ABS_OUT}/

PRE-FLIGHT CHECKS (do all THREE before terminating):

   1. ls ${SCALING_OUT}/*/real.md
      -> one per model size, each with all 4 strategies in the table.

   2. for f in ${SCALING_OUT}/*/real.md; do echo "=== \$f ==="; head -10 "\$f"; done
      -> sanity check the numbers per model.

   3. SCP everything to your laptop and verify it arrived:
      RUN ON YOUR LAPTOP (not the pod):

         scp -P <pod-ssh-port> -r root@${PUBLIC_IP}:${ABS_OUT} ./bench-results-scaling
         ls bench-results-scaling/*/real.md   # one per model

ON YOUR LAPTOP, AFTER SCP:

   # quick combined report (proper scaling-curve plot script comes next):
   for d in bench-results-scaling/*/; do
     short=\$(basename "\$d")
     echo "## \$short"; cat "\${d}real.md"; echo
   done > docs/results-scaling.md

   git add docs/results-scaling.md
   git commit -m "bench: scaling-curve cloud results across Qwen2.5 sizes"
   git push

THEN terminate the pod:

   - RunPod web dashboard -> your pod -> Terminate (NOT 'Stop')
   - or: runpodctl stop pod <pod-id>

================================================================================
EOF
