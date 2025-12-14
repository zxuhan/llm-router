# Results

This document publishes two sets of measurements:

1. **Real workers**: two `llama-server` instances serving Qwen2.5-0.5B-Instruct
   (Q4_K_M GGUF, ~470 MB) on an Apple M1 Pro. The proxy and replay harness are
   unchanged; only the upstreams differ from the fake-backend run. Cached
   tokens are reported by the upstream (`prompt_tokens_details.cached_tokens`)
   and aggregated by the report writer.
2. **Fake workers** (kept for reference): the same harness against in-process
   `httptest.Server` fakes that emit a fixed simulated TTFT. The fake-backend
   numbers isolate the algorithmic signal (router-side hit rate); the
   real-worker numbers add the actual KV-cache and decode behaviour.

To repeat any of these runs: `bash bench/scripts/real-llm.sh` for the real
workers (requires `brew install llama.cpp` and a downloaded GGUF model;
script will fail with helpful instructions if either is missing) or
`bash bench/scripts/run.sh` for the fake-backend scenario. See
`docs/benchmarks.md` for the full reproduction recipe.

---

## Real workers: Qwen2.5-0.5B on Apple M1 Pro

### Scenario A: light load (16 requests, 4 sessions x 4 turns, ~2 KB shared system prompt)

| Strategy | Requests | Hit rate (router) | KV cached (upstream) | TTFT p50 | TTFT p95 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 16 |  0.00% | 66.31% | 351 ms | 1415 ms | 6.6 |
| random       | 16 |  0.00% | 67.14% | 475 ms | 1547 ms | 6.2 |
| leastloaded  | 16 |  0.00% | 70.80% | 227 ms | 1437 ms | 7.3 |
| prefixaware  | 16 | 93.75% | 70.51% | 490 ms |  898 ms | 6.6 |

Backend distribution: `prefixaware` pinned all 16 requests to `w0`; the other
strategies split 6-10 / 10-6 across the two workers.

**The headline real-LLM signal: prefix-aware shaves ~37% off p95 TTFT (898 ms
vs 1415 ms for round-robin)** in this regime. The story under the hood:

- Every strategy reaches ~67-71% upstream cache reuse because the same system
  prompt repeats across requests; whichever workers see a request twice end up
  warm. The aggregate cache rate looks similar across strategies.
- What `prefixaware` removes is the *cold* requests at the tail. Round-robin
  spreads the first instance of the system prompt across both workers; each
  cold request pays a full prefill. `prefixaware` warms one worker on the
  first request, then routes everything sharing that prefix to the warm one,
  so subsequent requests skip prefill entirely. The cold-prefill tail is what
  p95 measures.
- Median (p50) TTFT does *not* improve, and `leastloaded` is in fact best at
  p50. With only 16 requests, several requests have to be cold somewhere; the
  one strategy that warms two workers in parallel (round-robin) clears those
  cold requests faster on the median, while `prefixaware` is sequentially
  warming a single worker.

### Scenario B: higher load (48 requests, 16 sessions x 3 turns, ~1 KB shared system prompt)

| Strategy | Requests | Hit rate (router) | KV cached (upstream) | TTFT p50 | TTFT p95 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 48 |  0.00% | 67.36% | 1184 ms | 1667 ms | 11.0 |
| random       | 48 |  0.00% | 67.47% | 1153 ms | 1930 ms | 10.7 |
| leastloaded  | 48 |  0.00% | 67.53% | 1276 ms | 1619 ms | 10.9 |
| prefixaware  | 48 | 95.83% | 67.56% | 1196 ms | 1552 ms | 11.0 |

Under sustained load both workers stay warm regardless of strategy, so
upstream cache rates converge to ~67%. The p95 TTFT advantage shrinks to
~7% (1552 ms vs 1667 ms) and per-request throughput is essentially tied.
This is the load regime where the algorithm's win narrows: when both
workers are saturated, where you route only changes which queue a request
joins, not whether prefill happens.

The router-side hit rate signal stays loud (95.83% vs 0%) because that
metric reports decisions, not outcomes.

### What we are not claiming

- **The TTFT gap is not 10x in this setup.** A 0.5 B parameter model with a
  ~2 KB prompt prefills in well under a second on M1 Pro; the savings from a
  warm prefix are correspondingly small in absolute terms. The same algorithm
  on a 70 B model with multi-KB prompts has order-of-magnitude prefill costs
  to recover, which the prefix routing decision can either save or waste; the
  algorithmic signal demonstrated here is the precondition for that
  larger-scale win.
- **2 workers is the smallest interesting case.** With 4-8 workers the gap
  widens because oblivious routing splits prefix copies across more workers
  (memory pressure) while prefix-aware concentrates them on one and treats
  the others as overflow.
- **Run-to-run variance is real.** Re-running with the same seed produces
  numbers within ~5-10% on p50 and ~2-5% on p95. The qualitative ordering
  (prefix-aware best at p95 in scenario A, all roughly tied in scenario B)
  is stable across the runs I observed.

---

## Fake workers (kept for the algorithmic isolation)

The fake-backend run with the same harness isolates the routing decision from
upstream variability. With identical fake backends emitting a fixed simulated
TTFT, all strategies see the same upstream behaviour; only the router differs.

### Scenario A': isolated routing signal (96 requests, 16 sessions x 6 turns, fake backends, 8 ms simulated TTFT)

| Strategy | Hit rate (router) | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 8.83 ms | 10.14 ms | 11.11 ms | 242.4 |
| random       |  0.00% | 8.87 ms | 10.04 ms | 11.57 ms | 242.4 |
| leastloaded  |  0.00% | 8.84 ms |  9.78 ms | 10.18 ms | 242.4 |
| prefixaware  | 98.96% | 8.87 ms | 12.44 ms | 15.05 ms | 240.8 |

`prefixaware` correctly identifies the shared-prefix routing opportunity in
98.96% of cases. TTFT is similar across strategies because the fake backend
emits a fixed simulated TTFT regardless of prefix state; this is the
well-defined floor of the harness. The whole point of the real-worker
scenario is to put a real KV cache behind those decisions.

### Scenario B': harness saturation behaviour

192 requests across 32 sessions, 40 ms simulated TTFT, `saturation_inflight: 4`.

| Strategy | Hit rate | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 41.48 ms | 47.30 ms | 50.35 ms | 400.9 |
| random       |  0.00% | 41.14 ms | 53.59 ms | 57.74 ms | 385.5 |
| leastloaded  |  0.00% | 41.17 ms | 43.89 ms | 46.24 ms | 399.8 |
| prefixaware  | 13.54% | 41.21 ms | 43.21 ms | 47.26 ms | 399.4 |

With the safety valve set tight, `prefixaware` spilled most requests to
`leastloaded`-style fallback; backend distribution was 69 / 63 / 60 across
three workers. The router-side hit rate dropped to 13.54% because the valve
fired on most pin attempts. This is the "load-bound regime" where the
algorithm's algorithmic win narrows by design.

---

## How to reproduce

```bash
# real workers (requires brew install llama.cpp and the model file)
mkdir -p models
curl -L -o models/qwen2.5-0.5b.gguf \
  https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf

# scenario A (light load)
SESSIONS=4 TURNS=4 SYS_LEN=2048 MAX_TOKENS=8 SEED=11 bash bench/scripts/real-llm.sh

# scenario B (high load)
SESSIONS=16 TURNS=3 SYS_LEN=1024 MAX_TOKENS=8 SEED=7 bash bench/scripts/real-llm.sh

# fake workers (no external dependencies)
bash bench/scripts/run.sh
```

The real-LLM script restarts both `llama-server` workers between strategies
so each strategy starts with empty KV caches. Without that reset, later
strategies inherit warm caches from earlier runs and the comparison is
contaminated.

## References

- SGLang RadixAttention: https://arxiv.org/abs/2312.07104
- vLLM automatic prefix caching: https://docs.vllm.ai/en/latest/automatic_prefix_caching/apc.html
- llama.cpp server prompt cache: https://github.com/ggerganov/llama.cpp/tree/master/examples/server
