#!/usr/bin/env bash
#
# One-shot pod setup for the cloud-vLLM benchmark.
#
# Designed for a fresh RunPod (or similar) instance with the PyTorch 2.x
# template (Ubuntu 22.04 / 24.04, CUDA 12.x already present, Python 3.10+).
#
# What it does:
#   1. Install Go 1.23 (only if not already present)
#   2. pip install vLLM (pinned)
#   3. Build the router binaries (`make build`)
#   4. Download the model from HuggingFace into models/
#
# Idempotent: re-running skips already-completed steps.
#
# Usage (from the repo root):
#   bash scripts/install-cloud.sh
#
# Tunables (env):
#   GO_VERSION   default 1.23.4
#   VLLM_VERSION default 0.6.4.post1
#   MODEL_ID     default Qwen/Qwen2.5-7B-Instruct
#   MODEL_DIR    default models/qwen2.5-7b
#   HF_HUB_ENABLE_HF_TRANSFER=1 is set for faster downloads.

set -euo pipefail

GO_VERSION="${GO_VERSION:-1.23.4}"
VLLM_VERSION="${VLLM_VERSION:-0.6.4.post1}"
MODEL_ID="${MODEL_ID:-Qwen/Qwen2.5-7B-Instruct}"
MODEL_DIR="${MODEL_DIR:-models/qwen2.5-7b}"

log() { printf "\n[install] %s\n" "$*"; }

# 1. Go ----------------------------------------------------------------------
if ! command -v go >/dev/null || [[ "$(go version 2>/dev/null)" != *"go${GO_VERSION%.[0-9]*}"* ]]; then
  log "installing Go ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
  export PATH="/usr/local/go/bin:${PATH}"
  if ! grep -q '/usr/local/go/bin' ~/.bashrc 2>/dev/null; then
    # shellcheck disable=SC2016
    echo 'export PATH=/usr/local/go/bin:$PATH' >> ~/.bashrc
  fi
else
  log "Go already installed: $(go version)"
fi
export PATH="/usr/local/go/bin:${PATH}"

# 2. vLLM --------------------------------------------------------------------
if ! python3 -c "import vllm" 2>/dev/null; then
  log "installing vLLM ${VLLM_VERSION}"
  pip install --upgrade pip --quiet
  # vLLM 0.6.4 was tested against the late-2024 transformers/hub versions.
  # Newer transformers (4.48+) removed `Tokenizer.all_special_tokens_extended`
  # which vllm 0.6.4 still calls; newer huggingface_hub (1.x) renamed enough
  # surface area to also break things. Pin both explicitly.
  pip install --quiet \
    "vllm==${VLLM_VERSION}" \
    "transformers==4.46.3" \
    "huggingface_hub==0.26.5" \
    "hf_transfer"
else
  installed=$(python3 -c "import vllm; print(vllm.__version__)" 2>/dev/null || echo unknown)
  log "vLLM already installed: ${installed}"
  # Even if vllm is already installed, repin transformers+hub in case the base
  # image / a previous step pulled in newer versions that break vllm 0.6.4.
  pip install --quiet "transformers==4.46.3" "huggingface_hub==0.26.5"
fi
# Make sure hf_transfer is present even if vllm install was skipped.
python3 -c "import hf_transfer" 2>/dev/null || pip install --quiet hf_transfer
export HF_HUB_ENABLE_HF_TRANSFER=1

# 3. Build router ------------------------------------------------------------
log "building router binaries"
make build >/dev/null

# 4. Model -------------------------------------------------------------------
mkdir -p "${MODEL_DIR}"
if [ -f "${MODEL_DIR}/config.json" ]; then
  log "model already present at ${MODEL_DIR}; skipping download"
else
  log "downloading ${MODEL_ID} -> ${MODEL_DIR}"
  # We pinned huggingface_hub to 0.26.5 above (so vllm 0.6.4 works); that
  # version still ships the `huggingface-cli` entrypoint, not `hf`.
  huggingface-cli download "${MODEL_ID}" \
    --local-dir "${MODEL_DIR}" \
    --local-dir-use-symlinks False
fi

# 5. Sanity check ------------------------------------------------------------
log "sanity check"
go version
python3 -c "import vllm; print('vllm', vllm.__version__)"
ls -lah "${MODEL_DIR}" | head -8
ls -lah bin/

log "ready. Next step: bash bench/scripts/cloud-vllm.sh"
