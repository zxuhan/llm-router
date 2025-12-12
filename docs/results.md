# Results

These numbers come from running `cmd/bench` against in-process fake
backends on an Apple Silicon laptop. The bench replaces real LLMs with a
deterministic HTTP simulator that emits a few SSE chunks after a
configurable simulated TTFT.

**Why fake backends.** The thesis under test is that the prefix-aware
routing strategy reuses prefix-cache state at a much higher rate than
oblivious strategies. That is an algorithmic claim about the *router*,
not about the inference engine: equal traffic into equal backends, the
router's choice is the only variable. Real workers vary on dozens of axes
(tokenizer, KV layout, context cap, batching) that obscure the algorithmic
signal. We therefore measure the algorithmic signal first, here, and call
out the limits clearly.

**What we explicitly do not claim.**

- We do not claim absolute throughput numbers comparable to vLLM or
  llama.cpp deployments.
- We do not claim TTFT speedups in microseconds at scale; the 8 ms TTFT
  in the harness below is the simulator's setpoint, not a real cold-start
  cost.
- We do not claim the hit-rate gap is the *same* on a 1 B and a 70 B
  model. The hit-rate gap is purely a function of the router's choices,
  so it transfers exactly. The *value* of that hit gap (TTFT savings) does
  scale with model size, because cold prefill is what KV caches save you
  from.

To run the same experiments yourself: `bash bench/scripts/run.sh` (and
see `docs/benchmarks.md`).

## Scenario A: light load, isolating the routing signal

96 requests across 16 sessions, 8 ms simulated TTFT per backend, default
safety valve (`saturation_inflight: 8`). With this load profile, no
backend ever reaches saturation, so the safety valve does not fire and
prefix-aware sends every match to the same worker that already had the
prefix.

| Strategy | Requests | Hit rate | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 96 |  0.00% | 8.83 ms | 10.14 ms | 11.11 ms | 242.4 |
| random       | 96 |  0.00% | 8.87 ms | 10.04 ms | 11.57 ms | 242.4 |
| leastloaded  | 96 |  0.00% | 8.84 ms |  9.78 ms | 10.18 ms | 242.4 |
| prefixaware  | 96 | 98.96% | 8.87 ms | 12.44 ms | 15.05 ms | 240.8 |

Backend distribution under `prefixaware`: all 96 requests landed on `w0`,
because the synthetic trace shares one system prompt across every
session. The router learned this on the first request and pinned every
subsequent shared-prefix prompt to the same worker.

This is the *correct* behaviour when the upstream cache pays off; the
hit-rate gap (98.96% vs 0%) is the algorithmic claim. With a real model
the TTFT for those 95 follow-on requests would collapse from a full
prefill to a single token's worth of compute.

## Scenario B: heavy load, the safety valve in action

192 requests across 32 sessions, 40 ms simulated TTFT per backend (slow
enough that requests pile up under fanout), `saturation_inflight: 4`.

| Strategy | Requests | Hit rate | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 192 |  0.00% | 41.48 ms | 47.30 ms | 50.35 ms | 400.9 |
| random       | 192 |  0.00% | 41.14 ms | 53.59 ms | 57.74 ms | 385.5 |
| leastloaded  | 192 |  0.00% | 41.17 ms | 43.89 ms | 46.24 ms | 399.8 |
| prefixaware  | 192 | 13.54% | 41.21 ms | 43.21 ms | 47.26 ms | 399.4 |

Backend distribution under `prefixaware`: w0=69, w1=63, w2=60. The safety
valve fired on most requests because pinning everything to w0 would have
queued up behind the saturation threshold; prefix-aware spread the load
while still picking up a 13.54% hit rate on the requests it could keep
local.

`leastloaded` and `prefixaware` end up at very similar p95/p99 because
the *load* signal dominates the *prefix* signal at this saturation level
on identical workers. On real heterogeneous workers (different model
sizes, different parallel settings) prefix-aware would still win on TTFT
even when it cannot win on hit rate.

## How to repeat these numbers

```bash
# scenario A
go run ./cmd/bench --seed 42 --sessions 16 --turns 6 \
  --system-len 768 --code-context-len 1536 --code-share 0.5 \
  --markdown docs/results.md

# scenario B
go run ./cmd/bench --seed 42 --sessions 32 --turns 6 \
  --system-len 768 --code-context-len 1536 --code-share 0.5 \
  --backend-ttft 40ms --saturation-inflight 4 \
  --markdown docs/results.md
```

Different seeds vary the trace; the qualitative comparison is stable
across seeds, the absolute numbers move by a few percent.

## Reading guide

- **Hit rate** is the headline number. It is unaffected by model size and
  represents how often the router successfully predicts a warm prefix.
- **TTFT delta** is the headline number on real workers. In this
  fake-backend harness the upstream returns in 8-40 ms regardless of
  prefix state, so the strategies tie within noise on TTFT. To see real
  TTFT savings, run the real-workers benchmark in `docs/benchmarks.md`.
- **Throughput** is similar across strategies in the harness because the
  simulator is the bottleneck. On real workers, prefix-aware raises
  throughput by avoiding redundant prefill compute.

## References

- SGLang RadixAttention: https://arxiv.org/abs/2312.07104
- vLLM automatic prefix caching: https://docs.vllm.ai/en/latest/automatic_prefix_caching/apc.html
- llama.cpp server prompt cache: https://github.com/ggerganov/llama.cpp/tree/master/examples/server
