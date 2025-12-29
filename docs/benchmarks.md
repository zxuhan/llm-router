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
scripts/bench/run.sh
```

This generates a deterministic synthetic trace, runs each strategy in turn
against three fake backends, and writes:

- `bench/results/results.json` (machine-readable; gitignored)
- `docs/results.md` (human-readable; the file you can commit if you want
  to publish your run)

Tunables (env vars consumed by `scripts/bench/run.sh`):

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

`scripts/bench/real-llm.sh` does the orchestration: it spins up two
`llama-server` instances against a small GGUF, runs each routing strategy
with FRESH workers (so KV caches start empty for each run), and aggregates
per-strategy JSON summaries into one Markdown report at
`bench/results/real.md`.

### 1. Install llama.cpp and download a small model

```bash
brew install llama.cpp                   # one-time
mkdir -p models                          # gitignored
curl -L -o models/qwen2.5-0.5b.gguf \
  https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf
```

The bench measures *relative* TTFT between strategies, so any GGUF works;
the 0.5 B Qwen model is the smallest reliably-available option.

### 2. Build and run

```bash
make build
bash scripts/bench/real-llm.sh
```

Outputs:
- `bench/results/real-<strategy>.json` per-strategy summary
- `bench/results/real.md` aggregated Markdown

### 3. Tunables (env vars)

| Var | Default | Meaning |
| --- | --- | --- |
| `MODEL` | `models/qwen2.5-0.5b.gguf` | path to the GGUF |
| `WORKER_PORTS` | `"8001 8002"` | space-separated list, accepts `N >= 2` |
| `SEED` | 42 | RNG seed for trace generation |
| `SESSIONS` | 8 | distinct sessions in the trace |
| `TURNS` | 4 | turns per session |
| `SYS_LEN` | 512 | shared system-prompt length in chars |
| `CODE_LEN` | 0 | per-session code-context length (0 disables) |
| `MAX_TOKENS` | 16 | upstream `max_tokens`; small to keep runs fast |
| `RUNS` | 1 | when > 1, runs each strategy N times with different seeds and emits mean ± stddev across runs |
| `CTX_SIZE` | 4096 | llama-server context size |

### 4. Multi-seed mode (recommended for headline numbers)

```bash
RUNS=3 SESSIONS=6 TURNS=3 SYS_LEN=2048 MAX_TOKENS=8 SEED=17 \
  bash scripts/bench/real-llm.sh
```

The aggregator picks up `--multi` automatically when `RUNS>1` and writes
a report with mean ± stddev for hit-rate, KV-cached, TTFT p50/p95/p99,
and RPS, plus a per-run detail table. Pool the raw per-request samples
into a single CDF with:

```bash
python3 scripts/plot/plot.py --input bench/results --out docs/images/cdf.png
```

### 5. Safety-valve ablation

```bash
SAT_VALUES="1 4 8 9999" bash scripts/bench/ablate-saturation.sh
```

Runs `prefixaware` four times with different `saturation_inflight`
values on the same trace, fresh workers per value. Output is a small
table in `bench/results/ablation/abl-sat.md` showing the cache-vs-load
tradeoff: tight saturation favours load distribution but hurts cache
reuse; loose saturation maximises cache reuse but bottlenecks throughput
on a single hot worker.

### 6. If you need a long-running router process

The orchestrator script bypasses `cmd/router` and runs the routing
in-process via `httptest`, because that's the fastest cycle for a
benchmark sweep. If you want to point real OpenAI clients at the router,
edit `config/config.yaml` so its `workers:` list points at the running
`llama-server` instances and start `bin/router`:

```bash
bin/router --config config/config.yaml
# clients hit http://127.0.0.1:8080/v1/chat/completions
```

## How to interpret the numbers

| Metric | What it means | Caveats on a small local box |
| --- | --- | --- |
| Hit rate | Fraction of successful requests where the chosen worker already held a non-zero prefix match. | Algorithmic; same on big and small models. |
| TTFT p50 | Median time-to-first-byte at the client. | On a fake backend or a tiny model the absolute number is dominated by inter-process latency, not model size. The *delta* between strategies is the signal. |
| TTFT p95 / p99 | Tail latency. | Saturated workers blow up the tail without the safety valve; that effect is observable in the harness. |
| RPS | Successful requests divided by wall-clock window of the run. | Low because traces are bursty by design. Look at relative differences. |

## Routing overhead microbenchmark

`internal/router/bench_test.go` measures the cost of `Router.Choose`
itself. Reproduce with:

```bash
go test -bench=. -benchmem -benchtime=2s -count=3 ./internal/router
```

Reference numbers on Apple M1 Pro (Go 1.23, `-race` off):

| Strategy | N backends | Prompt size | ns/op | allocs |
| --- | ---: | --- | ---: | ---: |
| roundrobin   |  2 | -        |     8 | 0 |
| roundrobin   | 16 | -        |    23 | 0 |
| random       |  4 | -        |    14 | 0 |
| leastloaded  | 16 | -        |    27 | 0 |
| prefixaware  |  4 | ~700 B   |  1.5 us | 49 |
| prefixaware  |  4 | ~4 KB    |  7.6 us | 256 |
| prefixaware  | 16 | ~4 KB    |  8.8 us | 256 |
| prefixaware (Update) | 4 | ~700 B | 2.0 us | 54 |

The non-prefix strategies are zero-allocation in the steady state thanks
to a fast path in the healthy-backend filter (see
`internal/router/router.go`). PrefixAware allocates the chunk-hash
sequence on each call; the per-chunk cost is dominated by FNV-1a
hashing and the radix-tree descent.

In every regime the routing overhead is dwarfed by the LLM TTFT itself
(milliseconds to seconds). At ~1000 req/s, even the 4 KB prompt-aware
case spends about 0.8% of CPU on routing decisions; the routing layer
is not the bottleneck.

## Useful reading

- SGLang's RadixAttention: https://arxiv.org/abs/2312.07104
- vLLM prefix caching documentation: https://docs.vllm.ai/en/latest/automatic_prefix_caching/apc.html
- llama.cpp `prompt-cache` docs: https://github.com/ggerganov/llama.cpp/tree/master/examples/server
