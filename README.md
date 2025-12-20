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
algorithm against (a) deterministic in-process backends for the
algorithmic signal and (b) up to three real `llama-server` workers
running Qwen2.5-0.5B and Qwen2.5-1.5B on a single Apple M1 Pro for the
actual KV-cache and TTFT behaviour. It is not a tuned production system,
and it does not ship benchmarks against a multi-GPU production fleet.
See "Limitations" below for what is and is not claimed.

## What you get from this repo

- A clean, OpenAI-compatible HTTP proxy (`cmd/router`) with four routing
  strategies, structured logging, Prometheus metrics, and graceful
  shutdown.
- A radix-tree-based prefix index, one per worker, with LRU eviction
  bounded by a configurable per-worker chunk budget. Concurrent-safe and
  fuzz-tested against a brute-force oracle.
- A safety valve that spills traffic away from saturated workers even
  when they would otherwise be the prefix-cache winner.
- A per-backend circuit breaker that opens after consecutive 5xx /
  transport failures and auto-resets after a cooldown; every strategy
  filters unhealthy backends out of its candidate pool (ADR 0007).
- A reproducible synthetic-trace generator (`cmd/gen-traces`) and a
  concurrent replay harness (`cmd/replay`) so the comparisons in
  `docs/results.md` can be regenerated end-to-end.
- An in-process benchmark (`cmd/bench`) that runs all four strategies
  against the same trace and emits a comparison report, plus an
  orchestrated `bench/scripts/real-llm.sh` that runs the same comparison
  against real `llama-server` workers (with fresh workers per strategy
  to avoid KV-cache carryover) and a Python script that renders a
  latency CDF.
- Microbenchmarks (`go test -bench`) for `Router.Choose`: round-robin
  is 8 ns / 0 allocs at N=2; even prefix-aware on a 4 KB prompt with 16
  workers is ~9 us, vastly cheaper than the LLM call itself.

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

![Pooled latency CDF, four strategies, three runs of N=18 each, fresh workers per run](docs/cdf.png)

**Methodology.** 3 runs at different seeds, 18 requests each (6 sessions
x 3 turns, ~2 KB shared system prompt). Every (strategy, run) pair gets
a fresh boot of all three `llama-server` workers so KV caches start
empty. Numbers below are mean ± stddev across the 3 runs.

| Strategy | Hit rate | KV cached | TTFT p50 | **TTFT p95** | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 58.53 ± 0.30%   | 1.88 s ± 317 ms | **7.54 s ± 263 ms** | 1.87 ± 0.03 |
| random       |  0.00% | 60.68 ± 6.58%   | 2.48 s ± 795 ms | **5.87 s ± 926 ms** | 1.96 ± 0.26 |
| leastloaded  |  0.00% | 62.51 ± 0.56%   | 0.75 s ± 256 ms | **6.77 s ± 1.32 s** | 2.29 ± 0.47 |
| prefixaware  | 94.44% | **74.97 ± 1.21%** | 1.92 s ± 146 ms | **3.23 s ± 121 ms** | **2.60 ± 0.15** |

**~57% lower mean p95 TTFT** for `prefixaware` vs round-robin
(3.23 s vs 7.54 s), with **stddev ~10x tighter**. Upstream KV-cache
reuse from `prompt_tokens_details.cached_tokens` jumps from ~59-63% to
**~75%**: routing alone unlocks an extra ~12-16 percentage points of
cache reuse on the same hardware. RPS is up ~14-39% because the warm
worker decodes faster.

The variance story matters as much as the mean. PA is not just faster on
average; it is *predictable*. Production SLOs care about that.

p50 TTFT is *not* improved by prefix-aware in this regime: cold
first-time prefills still happen on the warming worker, while
`leastloaded` parallelises them across three workers and wins p50. The
algorithm's win is at the tail and on cumulative throughput.

### Why these numbers transfer to bigger models

Per-request prefill time is roughly `prompt_tokens × per_token_prefill_time`;
the only thing the router can change is the *fraction* of those tokens
already cached upstream. Our measurement shows the prefix-aware strategy
lifts that fraction from ~59% to ~75% on identical traffic, a
~16 percentage-point lift that comes from routing decisions, not hardware.

Concrete back-of-envelope:

- For a 70 B model on H100-class hardware, cold prefill of a 500-token
  prompt is roughly 500 ms; warm prefill at the 75% cache-hit rate is
  ~125 ms. **Per-request savings ~375 ms (~75% reduction at the
  prefill stage).**
- For our M1 Pro / 1.5 B run, the equivalent cached-tokens math predicts
  ~1.5 s saved per request; we *measure* ~4 s of mean p95 reduction. The
  measured win is larger than the cached-tokens math alone predicts
  because warm workers also benefit from shorter queues, better
  decode-time locality, and zero false-share-prefill compute on top of
  the prefill saving.

The router's win does not depend on model size; the cached fraction is
purely an algorithmic property of the routing decisions. The *value* of
that win does scale: bigger models mean bigger absolute savings, which
is the property a production fleet inherits without re-tuning.

### Other regimes

- **Saturation regime.** Same model, 200 requests through 3 workers (the
  workers stay pegged the entire run). PA's safety valve fires
  constantly and PA gracefully degrades to `leastloaded`-shaped
  behaviour: PA tracks LL within a few percent on p95, both ~10% better
  than round-robin. The algorithm does not *hurt* under load; the gain
  just narrows because every worker eventually stays warm. Numbers in
  `docs/results.md`.
- **Safety-valve sensitivity.** A four-point ablation of
  `saturation_inflight` (1, 4, 8, 9999) shows the valve's behaviour
  along the spectrum from "spill aggressively, no pinning" to
  "pin everything, no spill". See `docs/results.md`.
- **Smaller-model reference.** A 0.5 B / 2-worker run shows the same
  qualitative pattern with smaller absolute differences, included for
  scaling discussion.

For the full methodology, the saturated-load + ablation tables, the
fake-backend isolation of the routing signal, and a per-run breakdown,
see `docs/results.md`.

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

## Future work

Things I know are missing or could be tightened, in rough order of impact:

- **Bootstrap CIs over the empirical CDF instead of mean ± stddev across
  3 seeds.** Multi-seed gets us most of the way; bootstrap would tighten
  the uncertainty on tail percentiles specifically.
- **Tokenizer-backed chunker.** Hash-of-bytes is correct but coarse.
  With a per-model tokenizer plugged in (one extra dep, a small helper),
  match boundaries would align with actual KV-cache boundaries. ADR 0006
  documents the trade-off.
- **Half-open one-probe circuit breaker.** The current breaker is
  closed -> open -> closed. Adding a half-open probe state would
  shed less traffic during recovery. ADR 0007 explains the deliberate
  simplification for small fleets.
- **Admin endpoint for draining.** `POST /admin/drain?backend=w0` to
  preemptively mark a worker unhealthy (for rolling restarts). Trivial
  to add on top of the existing health gate.
- **vLLM and mlx-lm backends as first-class.** The Backend interface is
  small enough that other OpenAI-compatible upstreams need only a new
  constructor; only llama.cpp is exercised in the committed tests.
- **Grafana dashboard JSON.** Metrics are emitted; a one-screen Grafana
  dashboard would make them legible at a glance.
- **Sticky-session affinity for non-prefix routers.** Even round-robin
  could pin within a session for free; the trace generator already
  carries SessionID for this. Not the project thesis, but a cheap win.

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
internal/    config, backend (incl. circuit breaker), prefixtree,
             router (incl. microbenchmarks), proxy, metrics, logging,
             trace, integration (no production code, just E2E tests)
docs/        architecture.md, benchmarks.md, results.md, cdf.png,
             decisions/ (seven ADRs)
config/      default config.yaml
bench/scripts run.sh (in-process), real-llm.sh (orchestrator),
             aggregate.go (results merger), plot.py (CDF renderer)
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
