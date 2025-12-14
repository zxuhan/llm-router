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
algorithm against deterministic in-process backends. It is not a tuned
production system, and it does not ship benchmarks against a real GPU
fleet. See "Limitations" below for what is and is not claimed.

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

1. Start two `llama-server` instances on different ports (with
   `--prompt-cache-all` enabled).
2. Edit `config/config.yaml` so the `workers:` list points at them.
3. Start the router:
   ```bash
   bin/router --config config/config.yaml
   ```
4. Generate a trace and replay:
   ```bash
   bin/gen-traces --out trace.jsonl
   bin/replay --trace trace.jsonl --out results.jsonl
   ```

Detailed steps and tunables are in `docs/benchmarks.md`.

## Headline numbers (in-process, fake backends)

| Strategy | Hit rate | TTFT p50 | TTFT p95 | RPS |
| --- | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% |  8.83 ms | 10.14 ms | 242.4 |
| random       |  0.00% |  8.87 ms | 10.04 ms | 242.4 |
| leastloaded  |  0.00% |  8.84 ms |  9.78 ms | 242.4 |
| prefixaware  | 98.96% |  8.87 ms | 12.44 ms | 240.8 |

Same trace, same backends, only the router differs. Hit rate is the
algorithmic signal; TTFT is similar because the fake backends emit a
fixed simulated TTFT regardless of prefix state. On real workers, the
hit-rate gap is what turns into a TTFT gap.

For the high-load scenario (where the safety valve fires) and the full
methodology, see `docs/results.md`.

## Limitations and caveats

- **Reference implementation, not a production system.** No
  authentication, no rate limiting, no graceful failover beyond the
  safety valve. Operate behind a real ingress.
- **Numbers in this repo are from in-process fake backends.** Replace
  the simulator with a real worker fleet (instructions in
  `docs/benchmarks.md`) to measure your hardware.
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
