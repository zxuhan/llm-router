# Results

This document publishes three sets of measurements:

1. **Real workers, multi-seed CIs**: three independent runs against three
   `llama-server` instances (Qwen2.5-1.5B, Q4_K_M, Apple M1 Pro). This is
   the canonical headline; numbers are mean ± stddev across seeds, with
   fresh workers per (strategy, run).
2. **Safety-valve ablation**: prefix-aware at four `saturation_inflight`
   values on the same trace, showing the cache-vs-load tradeoff.
3. **Saturation regime + smaller-model + fake-backend reference**:
   secondary scenarios that round out the picture.

To repeat any of these runs: `bash scripts/bench/real-llm.sh` (set
`RUNS=3` for multi-seed) or `bash scripts/bench/ablate-saturation.sh`
for the ablation. See `docs/benchmarks.md` for the full reproduction
recipe.

---

## 1. Headline: Qwen2.5-1.5B on Apple M1 Pro, 3 workers, multi-seed CIs

3 runs at seeds {17, 18, 19}, 18 requests each (6 sessions x 3 turns,
~2 KB shared system prompt). Every (strategy, run) combo gets a fresh
boot of all three workers so KV caches start empty. The CDF below pools
all 54 samples per strategy.

![Pooled latency CDF, 3 runs of N=18 each, fresh workers per run](images/cdf.png)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 57.56 ± 0.17% | 2.01 s ± 501 ms | 8.21 s ± 537 ms | 1.67 ± 0.05 |
| random       |  0.00% | 62.34 ± 2.22% | 1.70 s ± 461 ms | 8.06 s ± 921 ms | 1.84 ± 0.15 |
| leastloaded  |  0.00% | 61.94 ± 0.75% | 0.82 s ± 175 ms | 8.29 s ± 420 ms | 1.85 ± 0.08 |
| **prefixaware** | **94.44%** | **75.00 ± 1.12%** | 2.10 s ± 155 ms | **3.70 s ± 123 ms** | 2.36 ± 0.13 |

**Headline numbers:**

- **p95 TTFT cut by ~55% vs the best baseline (least-loaded)**, ~55%
  vs round-robin too (mean 3.70 s vs 8.21-8.29 s).
- **Stddev on p95 is 3-8x tighter for PA** (123 ms vs 420-921 ms across
  the three baselines). PA is the tightest p95 stddev of the four
  strategies; production SLOs care about that.
- **Upstream KV-cache hit rate** lifted from ~58-62% to **~75%** by
  routing decisions alone.
- **p50 TTFT is the worst of the four for PA**: 2.10 s vs 0.82 s for
  `leastloaded`. Pinning queues subsequent requests on the same worker
  while `leastloaded` parallelises cold prefills across three workers.
  The win is at the tail and on throughput (PA RPS 2.36 vs baselines
  1.67-1.85), not at p50.

### Per-run detail

PA's p95 sits in [3.62, 3.84] s across the three seeds (a 220 ms
window); the three baselines span 7.31-9.09 s.

| Run | RR p95 | Random p95 | LL p95 | **PA p95** |
| ---: | ---: | ---: | ---: | ---: |
| 1 (seed 17) |  7.81 s |  7.31 s |  7.81 s | **3.62 s** |
| 2 (seed 18) |  8.00 s |  9.09 s |  8.43 s | **3.63 s** |
| 3 (seed 19) |  8.82 s |  7.77 s |  8.62 s | **3.84 s** |

---

## 2. Safety-valve ablation

Prefix-aware at four `saturation_inflight` thresholds on a heavier
trace (12 sessions x 3 turns = 36 requests, same ~2 KB system prompt).
Fresh workers per run.

| saturation_inflight | Hit rate | Pinned | Spilled | Fallback | KV cached | TTFT p50 | TTFT p95 | RPS |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1                   | 11.11% |  2 | 2 | 32 | 71.06% | 3.93 s  | 20.36 s | 1.59 |
| 4                   | 91.67% | 33 | 0 |  3 | 67.88% | 6.79 s  | 15.85 s | 1.30 |
| 8 *(default)*       | 94.44% | 34 | 0 |  2 | 72.42% | 6.60 s  | 16.33 s | 1.22 |
| 9999 *(disabled)*   | 97.22% | 35 | 0 |  1 | 76.20% | 11.08 s | 14.02 s | 0.94 |

The table is the cache-vs-load tradeoff in one row each:

- **sat=1**: PA spills almost everything (32 fallbacks). Hit rate
  collapses, behaviour approaches `leastloaded`. Best p50 (3.93 s)
  because load distributes; worst p95 because the spilled requests pay
  cold prefill on previously-cold workers.
- **sat=4 / sat=8**: balanced. PA pins most of the trace; safety valve
  rarely fires; p50 climbs because the favoured worker queues; p95
  improves because cache hits remove cold prefills.
- **sat=9999**: valve effectively off. PA pins *everything*. Highest
  hit rate (97%) and highest cache reuse (76%); lowest RPS (0.94)
  because a single worker is the bottleneck. **Best p95 (14.0 s)**
  because every request after the first is fully warm; *worst* p50
  (11.1 s) because the 35 pinned requests serialise behind one worker.

The recommended default is `saturation_inflight=8`: pin enough to win
the cache-hit race, spill enough to not bottleneck on a single worker
when load spikes. Operators with tight tail SLOs and modest throughput
needs may prefer 9999 (best p95). Operators with throughput SLOs and
many shared prefixes may prefer 4.

---

## 3. Saturation regime: 200 requests through the same fleet

Single run, 50 sessions x 4 turns = 200 requests. Both upstream queues
stay pegged the whole run; this is the stress case, not the design point.

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 79.73% | 14.23 s | 21.44 s | 3.15 |
| random       |  0.00% | 79.47% | 12.34 s | 21.56 s | 3.19 |
| leastloaded  |  0.00% | 79.37% | 11.60 s | 18.91 s | 3.22 |
| prefixaware  | **11.50%** | 79.47% | 11.20 s | 19.25 s | 3.19 |

What this shows:

- **Cache hit rates converge** to ~79% across all strategies because
  every worker eventually warms up under sustained load. The router's
  effect on cache reuse is bounded above by "everyone caches eventually".
- **PA's hit rate drops to 11.5%** because the safety valve fires on 177
  of 200 requests (the worker holding the prefix is constantly above
  saturation). PA gracefully degrades to `leastloaded`-shaped routing.
- PA still wins on p95 vs round-robin (19.25 s vs 21.44 s) and ties
  `leastloaded` (18.91 s). The algorithm does not *hurt* under load; the
  win just narrows.

This is the regime where prefix-awareness has the least to give: when
every worker is already warm and saturated, the routing decision only
changes which queue a request joins, not whether prefill happens.

---

## 4. Smaller-model reference: Qwen2.5-0.5B, 2 workers

Same harness, smaller model, fewer workers. 16 requests, 4 sessions x 4
turns, ~2 KB shared system prompt.

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 66.31% | 351 ms | 1415 ms | 6.6 |
| random       |  0.00% | 67.14% | 475 ms | 1547 ms | 6.2 |
| leastloaded  |  0.00% | 70.80% | 227 ms | 1437 ms | 7.3 |
| prefixaware  | 93.75% | 70.51% | 490 ms |  898 ms | 6.6 |

The 0.5 B run shows the same qualitative pattern (PA wins p95 by ~37%)
with smaller absolute differences. Cache rates converge across
strategies because with only 2 workers, every strategy ends up warming
both within a few requests. The 1.5 B / 3-worker headline above
demonstrates the cache-rate gap that opens up when there are enough
workers for PA to *not* spread the prefix.

---

## 5. Fake-backend run (algorithmic isolation, no real model)

The fake-backend run isolates the routing decision from upstream
variability. With identical fake backends emitting a fixed simulated
TTFT, all strategies see the same upstream behaviour; only the router
differs.

96 requests, 16 sessions x 6 turns, fake backends, 8 ms simulated TTFT:

| Strategy | Hit rate | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| --- | ---: | ---: | ---: | ---: | ---: |
| roundrobin   |  0.00% | 8.83 ms | 10.14 ms | 11.11 ms | 242.4 |
| random       |  0.00% | 8.87 ms | 10.04 ms | 11.57 ms | 242.4 |
| leastloaded  |  0.00% | 8.84 ms |  9.78 ms | 10.18 ms | 242.4 |
| prefixaware  | 98.96% | 8.87 ms | 12.44 ms | 15.05 ms | 240.8 |

`prefixaware` correctly identifies the shared-prefix routing
opportunity in 98.96% of cases. TTFT is similar across strategies
because the fake backend emits a fixed simulated TTFT regardless of
prefix state; this is the well-defined floor of the harness.

---

## What we are not claiming

- **The savings are not "free", they are at the tail.** Prefix-aware
  trades load distribution for cache locality. p50 TTFT is *not*
  improved here. The win is at p95/p99 and on cumulative throughput,
  where avoiding redundant prefill compounds.
- **Hardware matters.** All numbers are on a single Apple M1 Pro. The
  16 GB unified memory just fits three Q4_K_M 1.5 B model instances; on
  a smaller machine the workers contend and numbers compress. The
  algorithm is the same on a multi-GPU production fleet; bigger models
  yield larger absolute savings because prefill cost scales with model
  size while the routing decision does not.
- **N=18 per run is small.** We compensated with multi-seed CIs (3
  runs, mean ± std). A production benchmark would use thousands of
  requests and report bootstrap CIs over the empirical CDF. We
  deliberately kept the per-run trace small so the harness reproduces
  in minutes on a laptop.

## Why these numbers transfer to bigger models

Per-request prefill time is roughly `prompt_tokens × per_token_prefill_time`;
the only thing the router can change is the fraction of those tokens
already cached upstream. Our measurement shows prefix-aware lifts that
fraction from ~58-62% (baselines) to ~75% on identical traffic, a
13 percentage-point lift over `leastloaded` (the strongest baseline)
that comes from routing decisions, not hardware.

Concrete back-of-envelope:

- For a 70 B model on H100-class hardware, cold prefill of a 500-token
  prompt is roughly 500 ms; warm prefill at the 75% cache-hit rate is
  ~125 ms. **Per-request savings ~375 ms (~75% reduction at the
  prefill stage).**
- For our M1 Pro / 1.5 B run, the cached-tokens math predicts ~1.5 s
  saved per request; we *measure* ~4.6 s of mean p95 reduction
  (8.29 s LL vs 3.70 s PA). The measured win exceeds the prefill-only
  estimate because warm workers also benefit from shorter queues and
  better decode-time locality on top of the prefill saving.

The router's win does not depend on model size. The *value* of that win
does scale: bigger models mean bigger absolute savings, which is the
property a production fleet inherits without re-tuning.

---

## How to reproduce

```bash
brew install llama.cpp                         # one-time
mkdir -p models                                # gitignored

# headline (multi-seed): Qwen2.5-1.5B, 3 workers, RUNS=3
curl -L -o models/qwen2.5-1.5b.gguf \
  https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf
MODEL=models/qwen2.5-1.5b.gguf \
  WORKER_PORTS="8001 8002 8003" \
  SESSIONS=6 TURNS=3 SYS_LEN=2048 MAX_TOKENS=8 SEED=17 \
  RUNS=3 \
  bash scripts/bench/real-llm.sh
/path/to/python scripts/plot/plot.py --input bench/results --out docs/images/cdf.png

# safety-valve ablation
SAT_VALUES="1 4 8 9999" bash scripts/bench/ablate-saturation.sh

# saturation regime (single N=200 run)
SESSIONS=50 TURNS=4 SYS_LEN=2048 MAX_TOKENS=8 SEED=17 \
  bash scripts/bench/real-llm.sh

# smaller-model reference
curl -L -o models/qwen2.5-0.5b.gguf \
  https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf
SESSIONS=4 TURNS=4 SYS_LEN=2048 MAX_TOKENS=8 SEED=11 bash scripts/bench/real-llm.sh

# fake workers (no external dependencies)
bash scripts/bench/run.sh
```

The real-LLM script restarts every `llama-server` worker between
strategies (and between runs in multi-seed mode) so each starts with
empty KV caches. Without that reset, later strategies inherit warm
caches from earlier runs and the comparison is contaminated.

`WORKER_PORTS` is a space-separated list and accepts any `N >= 2`. Each
worker takes ~1 GB of resident memory for the 1.5 B Q4_K_M GGUF; budget
accordingly.

## References

- SGLang RadixAttention: https://arxiv.org/abs/2312.07104
- vLLM automatic prefix caching: https://docs.vllm.ai/en/latest/features/automatic_prefix_caching.html
- llama.cpp server prompt cache: https://github.com/ggerganov/llama.cpp/tree/master/tools/server
