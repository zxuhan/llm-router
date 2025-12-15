# llm-router

A prefix-cache aware reverse proxy for OpenAI-compatible LLM inference
servers. The router speaks `/v1/chat/completions` on the front, forwards
to N llama.cpp / mlx-lm / vLLM workers on the back, and biases routing
toward the worker whose KV cache is most likely to already hold the
request's prefix.

## Thesis

When agentic traffic shares prompts (a system prompt across many
sessions, a multi-turn conversation that grows the same context, a
ReAct-style tool loop on top of a fixed instruction block), routing each
request to the worker that already has the relevant KV-cache state
produces order-of-magnitude TTFT improvements. The routing logic is
algorithmic and independent of model size: a 95% cache hit rate vs. a 12%
cache hit rate looks the same on a 1 B model as on a 70 B model, and the
*value* of those hits scales up with model size because cold prefill is
what KV caches save you from.

This repository is a **reference implementation** that demonstrates the
algorithm against (a) deterministic in-process backends for the algorithmic
signal and (b) two real `llama-server` workers running Qwen2.5-0.5B on
Apple M1 Pro for the actual KV-cache and TTFT behaviour. It is not a tuned
production system, and it does not ship benchmarks against a multi-GPU
production fleet. See "Limitations" below for what is and is not claimed.

## What you get from this repo

- A clean, OpenAI-compatible HTTP proxy (`cmd/router`) with four routing
  strategies, structured logging, Prometheus metrics, and graceful
  shutdown.
- A radix-tree-based prefix index, one per worker, with LRU eviction
  bounded by a configurable per-worker chunk budget. Concurrent-safe and
  fuzz-tested against a brute-force oracle.
- A safety valve that spills traffic away from saturated workers even
  when they would otherwise be the prefix-cache winner.
- A reproducible synthetic-trace generator (`cmd/gen-traces`) and a
  concurrent replay harness (`cmd/replay`) so the comparisons in
  `docs/results.md` can be regenerated end-to-end.
- An in-process benchmark (`cmd/bench`) that runs all four strategies
  against the same trace and emits a comparison report. No external
  workers required.

## Architecture (one screen)

```text
                        +---------------------+
   client --POST-->     | router (Go service) | --HTTP/SSE--> worker A (llama.cpp)
   /v1/chat/completions |  - prompt extract   |
                        |  - strategy.Choose  | --HTTP/SSE--> worker B (llama.cpp)
                        |  - tree.Update      |
                        |  - SSE pass-through | --HTTP/SSE--> worker C (mlx-lm.server)
                        +---------------------+
                                 |
                                 +-- /metrics  (prometheus)
                                 +-- /healthz
```

Routing strategies are pluggable; the proxy and backends are unaware of
which one is in use.

| Strategy | Picks based on | Notes |
| --- | --- | --- |
| `roundrobin` | rotation counter | baseline |
| `random` | uniform random | baseline |
| `leastloaded` | min `Inflight()` | baseline; reacts to load |
| `prefixaware` | longest prefix match per worker, with a saturation safety valve | the headline strategy |

See `docs/architecture.md` for the deeper view (sequence diagrams, package
graph, concurrency model).

## Quickstart

### A. Run the in-process bench (no external workers)

```bash
make build
bench/scripts/run.sh
$EDITOR docs/results.md
```

This runs every routing strategy against the same synthetic agent trace
using deterministic fake backends. The hit-rate comparison is the
algorithmic signal; absolute TTFT is the simulator's setpoint, not a real
model number.

### B. Run against real llama.cpp workers

The fastest path is the orchestrated script: it spins up two `llama-server`
instances, runs each strategy with fresh workers (so KV caches start
empty per strategy), and emits a comparison report.

```bash
brew install llama.cpp                   # one-time
mkdir -p models                          # one-time; gitignored
curl -L -o models/qwen2.5-1.5b.gguf \
  https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf
make build
MODEL=models/qwen2.5-1.5b.gguf \
  WORKER_PORTS="8001 8002 8003" \
  SESSIONS=6 TURNS=3 SYS_LEN=2048 MAX_TOKENS=8 SEED=17 \
  bash bench/scripts/real-llm.sh
$EDITOR bench/results/real.md
```

Tunables: `MODEL`, `WORKER_PORTS`, `SESSIONS`, `TURNS`, `SYS_LEN`,
`MAX_TOKENS`, `SEED`, `CTX_SIZE` are all environment overrides.
`WORKER_PORTS` is a space-separated list and accepts any `N >= 2`.

For a long-running router process (rather than per-strategy bench
subprocesses), point `config/config.yaml` at the workers and run
`bin/router --config config/config.yaml`; clients then talk OpenAI to
`http://127.0.0.1:8080/v1/chat/completions` directly.

Detailed steps and tunables are in `docs/benchmarks.md`.

## Headline numbers (real `llama-server`, Qwen2.5-1.5B, M1 Pro, 3 workers)

18 requests, 6 sessions x 3 turns, ~2 KB shared system prompt. All three
workers restarted between strategies so each starts with empty KV caches.

| Strategy | Hit rate (router) | KV cached (upstream) | TTFT p50 | **TTFT p95** | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 58.00% | 2.44 s | **10.28 s** | 1.38 |
| random       |  0.00% | 63.26% | 1.54 s | **9.29 s**  | 1.57 |
| leastloaded  |  0.00% | 62.99% | 0.61 s | **10.14 s** | 1.59 |
| prefixaware  | 94.44% | **76.31%** | 2.59 s | **4.04 s**  | **1.98** |

**~60% lower p95 TTFT** for `prefixaware` vs round-robin (4.04 s vs
10.28 s). Upstream KV-cache reuse from `prompt_tokens_details.cached_tokens`
jumps from ~58-63% to **76%**: routing alone unlocks an extra ~13-18
percentage points of cache reuse on the same hardware. RPS is up ~25-43%
because the warm worker decodes faster.

p50 TTFT is *not* improved by prefix-aware in this regime: cold
first-time prefills still happen on the warming worker, while
`leastloaded` parallelises them across three workers and wins p50. The
algorithm's win is at the tail and on cumulative throughput.

For the smaller-model run (0.5B, 2 workers), the saturated-load regime
where the win narrows, the safety-valve behaviour, fake-backend isolation
of the routing signal, and the full methodology, see `docs/results.md`.

## Limitations and caveats

- **Reference implementation, not a production system.** No
  authentication, no rate limiting, no graceful failover beyond the
  safety valve. Operate behind a real ingress.
- **Real-worker numbers are from 0.5B / 1.5B Qwen models on a single
  M1 Pro.** The algorithm is the same on a 70 B model; the absolute TTFT
  savings scale with prefill cost (which grows roughly linearly with
  model size). What is demonstrated here is the qualitative win at the
  tail and the router-side hit-rate signal.
- **Tokenisation is approximated by chunk-hashing**, not a real
  tokenizer. ADR 0006 explains the trade-off; for byte-for-byte shared
  prefixes (which is what KV caches actually key on) the approximation
  is correct.
- **No KV-budget renegotiation.** The configured `kv_budget` per worker
  is static. A worker whose cache shrinks under memory pressure will
  silently disagree with the router's prediction; the only consequence
  is occasional false-positive prefix matches, which self-correct as the
  LRU ages out.
- **The model-agnostic hit-rate claim transfers exactly to large
  models.** The TTFT-savings claim transfers as roughly proportional to
  prefill cost, which grows roughly linearly with model size. Concrete
  numbers depend on your hardware and model.

## Configuration

`config/config.yaml` is the only required input. Every field can be
overridden by an environment variable with the prefix `ROUTER_` (see
`internal/config/config.go` for the whitelist). Operator concerns to look
at first:

- `router.strategy` (start with `prefixaware`).
- `router.chunk_size` (default 32 bytes; see ADR 0006).
- `router.min_match_chunks` (how confident the prefix match must be
  before we pin; default 2).
- `router.saturation_inflight` (when the safety valve kicks in; default
  8).
- `workers[].kv_budget` (the soft chunk budget per worker; should
  loosely correspond to that worker's KV cache capacity).

## Repository layout

```text
cmd/         router, replay, gen-traces, bench (CLI binaries)
internal/    config, backend, prefixtree, router, proxy, metrics, logging,
             trace, integration (no production code, just E2E tests)
docs/        architecture.md, benchmarks.md, results.md, decisions/ (ADRs)
config/      default config.yaml
bench/       scripts (results/ is gitignored)
.github/     ci workflow
```

## Development

```bash
make build         # builds all four CLI binaries into bin/
make test          # go test
make test-race     # go test -race
make cover         # coverage summary on internal/...
make lint          # golangci-lint
make bench         # runs bench/scripts/run.sh
```

## Acceptance bar (verified locally)

- `make build` succeeds on a clean checkout.
- `make test-race` passes with no data races.
- `go test ./internal/... -cover -covermode=atomic` reports >= 90% on
  every `internal/` package.
- `golangci-lint run` is clean.
- The router handles streaming SSE end-to-end against the fake backend
  (proven by `internal/integration/streaming_test.go`).
- `cmd/gen-traces` produces a valid trace; `cmd/replay` replays it; in
  the harness `prefixaware` beats every other strategy on hit rate.

## Further reading

- SGLang's RadixAttention: https://arxiv.org/abs/2312.07104
- vLLM automatic prefix caching: https://docs.vllm.ai/en/latest/automatic_prefix_caching/apc.html
- llama.cpp prompt cache: https://github.com/ggerganov/llama.cpp/tree/master/examples/server

## License

MIT.
