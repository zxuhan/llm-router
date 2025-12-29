#!/usr/bin/env bash
#
# Run the in-process benchmark harness across all routing strategies and
# write reports to docs/. The harness uses fake backends (no llama.cpp boot
# required), so this script is safe to run on any machine with Go installed.
#
# Outputs:
#   bench/results/results.json
#   docs/results.md   (overwritten; commit only when you want to publish)
#
# Tunables: pass through to cmd/bench (see --help). Common knobs:
#   SESSIONS, TURNS, SYS_LEN, CODE_LEN, CHUNK_SIZE.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

OUT_DIR="bench/results"
mkdir -p "$OUT_DIR"

SEED="${SEED:-42}"
SESSIONS="${SESSIONS:-12}"
TURNS="${TURNS:-6}"
SYS_LEN="${SYS_LEN:-768}"
CODE_LEN="${CODE_LEN:-1536}"
CHUNK_SIZE="${CHUNK_SIZE:-32}"

go run ./cmd/bench \
  --seed "$SEED" \
  --sessions "$SESSIONS" \
  --turns "$TURNS" \
  --system-len "$SYS_LEN" \
  --code-context-len "$CODE_LEN" \
  --chunk-size "$CHUNK_SIZE" \
  --json "$OUT_DIR/results.json" \
  --markdown "docs/results.md"

echo
echo "Wrote $OUT_DIR/results.json and docs/results.md"
