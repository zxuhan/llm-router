# Benchmarks

This document describes how to reproduce the comparison numbers in
`results.md`. Two harnesses are provided:

1. **`cmd/bench`** runs every routing strategy against an in-process fake
   backend that simulates a small constant TTFT. No external workers
   needed; this is the harness committed numbers come from. Strategy *hit
   rate* differences here reflect the algorithm exactly, since every
   backend is identical.
2. **`cmd/replay` plus a real router instance** for benchmarks against a
   live llama.cpp / mlx-lm fleet. This is the path that produces real TTFT
   numbers on your hardware.

## In-process benchmark (recommended for the hit-rate comparison)

```bash
make build
bench/scripts/run.sh
```

This generates a deterministic synthetic trace, runs each strategy in turn
against three fake backends, and writes:

- `bench/results/results.json` (machine-readable; gitignored)
- `docs/results.md` (human-readable; the file you can commit if you want
  to publish your run)

Tunables (env vars consumed by `bench/scripts/run.sh`):

| Var | Default | Meaning |
| --- | --- | --- |
| `SEED` | 42 | trace RNG seed |
| `SESSIONS` | 12 | distinct sessions in the trace |
| `TURNS` | 6 | turns per session |
| `SYS_LEN` | 768 | system-prompt length in chars |
| `CODE_LEN` | 1536 | code-context length (0 disables) |
| `CHUNK_SIZE` | 32 | prefix-aware chunk size |

Run `go run ./cmd/bench --help` for the full flag list (including
`--saturation-inflight`, `--min-match-chunks`, and the simulated
`--backend-ttft`).

## Real-workers benchmark

This path measures actual KV-cache reuse on your laptop or workstation. It
needs at least two llama.cpp instances on different ports.

### 1. Start two workers

```bash
# in two terminals
llama-server -m models/Qwen2.5-0.5B-Instruct-Q4_K_M.gguf --port 8001 \
  --ctx-size 8192 --parallel 4 --prompt-cache-all
llama-server -m models/Qwen2.5-0.5B-Instruct-Q4_K_M.gguf --port 8002 \
  --ctx-size 8192 --parallel 4 --prompt-cache-all
```

(Use whatever model file you have. The bench measures *relative* TTFT
between strategies, so the absolute model speed does not affect the
comparison.)

### 2. Configure the router

Edit `config/config.yaml` so its `workers:` section points at the two
ports above. Leave `strategy: prefixaware` for the first run.

### 3. Run the router

```bash
bin/router --config config/config.yaml
```

### 4. Generate a trace and replay it

```bash
bin/gen-traces --out trace.jsonl --sessions 16 --turns 8 \
  --system-len 1024 --code-context-len 2048
bin/replay --trace trace.jsonl --endpoint http://127.0.0.1:8080/v1/chat/completions \
  --out results-prefixaware.jsonl
```

### 5. Repeat for each strategy

Set `router.strategy: roundrobin` (and `random`, `leastloaded`) in the
config, restart the router, and rerun `bin/replay --out
results-<strategy>.jsonl`. Use the same `trace.jsonl` for all runs.

### 6. Aggregate

Use `bin/bench` if you want the same Markdown table format as the
in-process run: it expects in-process backends, so for cross-strategy
comparison from the JSONL outputs above you can write a small script that
reads each `results-*.jsonl`, calls `trace.Summarise(name, results)` for
each, and emits the report. (The harness library functions are public.)

## How to interpret the numbers

| Metric | What it means | Caveats on a small local box |
| --- | --- | --- |
| Hit rate | Fraction of successful requests where the chosen worker already held a non-zero prefix match. | Algorithmic; same on big and small models. |
| TTFT p50 | Median time-to-first-byte at the client. | On a fake backend or a tiny model the absolute number is dominated by inter-process latency, not model size. The *delta* between strategies is the signal. |
| TTFT p95 / p99 | Tail latency. | Saturated workers blow up the tail without the safety valve; that effect is observable in the harness. |
| RPS | Successful requests divided by wall-clock window of the run. | Low because traces are bursty by design. Look at relative differences. |

## Useful reading

- SGLang's RadixAttention: https://arxiv.org/abs/2312.07104
- vLLM prefix caching documentation: https://docs.vllm.ai/en/latest/automatic_prefix_caching/apc.html
- llama.cpp `prompt-cache` docs: https://github.com/ggerganov/llama.cpp/tree/master/examples/server
