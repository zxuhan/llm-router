# Cloud results: 4× A100 + vLLM + Qwen2.5

End-to-end numbers from the canonical cloud benchmark. The runbook for
reproducing this is in [cloud-bench.md](cloud-bench.md). The script that
produced the data is [`scripts/bench/concurrency-sweep.sh`](../scripts/bench/concurrency-sweep.sh).

## Setup

| | Value |
| :--- | :--- |
| Provider | RunPod community cloud |
| Hardware | 4× NVIDIA A100 80GB SXM4 |
| Upstream | vLLM 0.6.4.post1 with `--enable-prefix-caching` and `--enable-prompt-tokens-details` |
| Models | Qwen2.5-7B-Instruct (FP16), Qwen2.5-14B-Instruct (FP16) |
| Concurrency points | 4, 8, 12, 16, 24 sessions |
| Per-point repetitions | 3 seeds × 4 strategies = 12 mini-benches |
| Trace shape | 6 KB shared system prompt, 8 turns per session, max_tokens=64 |
| Worker boot | fresh vLLM per (strategy, seed); KV cache always starts empty |
| Routing | proxy in process, 4 backends, default `saturation_inflight=4` |

Total wall time per model: ~1.5-2 hours. Cost at $5.97/hr for 4× A100 SXM:
~$10-13 per model size, ~$25 for both.

## Headline: TTFT slope under load

The single most important property: how does each strategy's TTFT grow as
concurrency rises from "1 session per worker" (sessions=4) to "6 sessions
per worker" (sessions=24)?

| Strategy | 7B slope | 14B slope |
| :--- | ---: | ---: |
| Random           | +49 ms | +98 ms |
| Round-robin      | +31 ms | +36 ms |
| Least-loaded     | +35 ms | +50 ms |
| **Prefix-aware** | **+16 ms** | **+25 ms** |

Prefix-aware has the gentlest slope on both model sizes. Slope ratios
relative to PA: random 3.0× (7B) / 3.9× (14B); least-loaded 2.2× / 2.0×;
round-robin 1.9× / 1.4×. Predictable latency under load is the
production-grade SLA property.

![Concurrency sweep at Qwen2.5-7B and Qwen2.5-14B on 4× A100. PA stays flat under load; baselines climb 2-3× faster.](images/hero-cloud.png)

## Per-concurrency tables

All numbers are mean ± stddev across 3 seeds. KV cached % is the
upstream-reported `prompt_tokens_details.cached_tokens / prompt_tokens`,
averaged over successful requests.

### Qwen2.5-7B-Instruct

#### sessions=4 (1 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 80.30 ± 0.62% |  84 ms |  861 ms | 1.02 s | 14.6 |
| random       | 0.00% | 80.94 ± 0.49% |  98 ms |  991 ms | 1.04 s | 14.1 |
| **leastloaded**  | 0.00% | 85.15 ± 0.28% |  **66 ms** |  833 ms |  956 ms | **15.7** |
| prefixaware  | 96.88% | **94.19 ± 0.19%** |  83 ms |  995 ms | 1.07 s | 13.4 |

#### sessions=8 (2 per worker)

The `real.md` aggregate for this point was lost when a transient vLLM
worker crash hit the very last seed; per-strategy JSONs survive and feed
the chart. Strategy-by-strategy from the surviving seeds:

| Strategy | KV cached | TTFT p50 |
| :--- | ---: | ---: |
| roundrobin   | ~87% |  ~87 ms |
| random       | ~88% | ~100 ms |
| **leastloaded**  | ~91% |  **~70 ms** |
| prefixaware  | **~94%** |  ~84 ms |

#### sessions=12 (3 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 89.55 ± 0.28% |  91 ms | 1.06 s | 1.15 s | 34.8 |
| random       | 0.00% | 89.17 ± 0.04% | 110 ms | 1.16 s | 1.36 s | 30.3 |
| **leastloaded**  | 0.00% | 92.26 ± 0.60% |  **87 ms** |  975 ms | 1.10 s | **39.5** |
| prefixaware  | 96.88% | **94.20 ± 0.15%** |  94 ms | **1.03 s** | 1.21 s | 36.7 |

#### sessions=16 (4 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 90.41 ± 0.23% |  95 ms | 1.10 s | 1.26 s | 43.8 |
| random       | 0.00% | 90.13 ± 0.27% | 122 ms | 1.11 s | 1.32 s | 37.4 |
| leastloaded  | 0.00% | 93.07 ± 0.21% |  88 ms | 1.01 s | 1.15 s | 43.7 |
| **prefixaware**  | 96.88% | **94.18 ± 0.16%** |  **87 ms** | **1.01 s** | **1.15 s** | **44.3** |

#### sessions=24 (6 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 91.31 ± 0.41% | 115 ms | 1.31 s | 1.48 s | 47.3 |
| random       | 0.00% | 90.92 ± 0.19% | 146 ms | 1.26 s | 1.61 s | 47.0 |
| leastloaded  | 0.00% | 94.60 ± 0.21% | 102 ms | **1.20 s** | 1.38 s | **55.8** |
| **prefixaware**  | 55.56 ± 2.10% | **94.97 ± 0.27%** |  **99 ms** |  1.18 s | **1.37 s** | 53.9 |

(PA's hit rate dropped from 97% to 56% at sessions=24 because the safety
valve aggressively spilled to other workers under saturation. **By design**:
the spilling is what kept TTFT in the lead.)

### Qwen2.5-14B-Instruct

#### sessions=4 (1 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 80.04 ± 0.79% | 157 ms | 1.78 s | 1.83 s | 6.0 |
| random       | 0.00% | 81.43 ± 0.58% | 167 ms | 1.82 s | 1.94 s | 5.2 |
| **leastloaded**  | 0.00% | 85.41 ± 0.52% | **122 ms** | **1.68 s** | 1.78 s | 5.8 |
| prefixaware  | 96.88% | **94.19 ± 0.19%** | 141 ms | 1.89 s | 2.03 s | 5.3 |

#### sessions=8 (2 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 87.17 ± 1.01% | 155 ms | 1.72 s | 1.94 s | 10.4 |
| random       | 0.00% | 87.37 ± 0.31% | 172 ms | 1.86 s | 2.06 s | 10.5 |
| **leastloaded**  | 0.00% | 90.82 ± 0.67% | **132 ms** | 1.73 s | 1.94 s | 10.9 |
| prefixaware  | 96.88% | **94.34 ± 0.16%** | 140 ms | 1.84 s | 2.09 s | 10.5 |

#### sessions=12 (3 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 88.90 ± 0.29% | 161 ms | 1.94 s | 2.09 s | 14.9 |
| random       | 0.00% | 89.42 ± 0.26% | 186 ms | 1.99 s | 2.13 s | 14.4 |
| **leastloaded**  | 0.00% | 92.39 ± 0.27% | 140 ms | 1.84 s | 1.96 s | **15.5** |
| **prefixaware**  | 96.88% | **94.19 ± 0.15%** | **141 ms** | **1.85 s** | 2.11 s | 15.1 |

#### sessions=16 (4 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 89.99 ± 0.39% | 171 ms | 2.06 s | 2.21 s | 17.4 |
| random       | 0.00% | 89.93 ± 0.42% | 229 ms | 2.07 s | 2.38 s | 17.4 |
| leastloaded  | 0.00% | 93.38 ± 0.27% | 152 ms | 1.88 s | **2.02 s** | **20.7** |
| **prefixaware**  | 96.88% | **94.18 ± 0.16%** | **148 ms** | **1.85 s** | 2.03 s | 20.0 |

#### sessions=24 (6 per worker)

| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| roundrobin   | 0.00% | 91.10 ± 0.37% | 193 ms | 2.40 s | 2.72 s | 25.2 |
| random       | 0.00% | 90.69 ± 0.22% | 265 ms | 2.36 s | 2.74 s | 23.4 |
| leastloaded  | 0.00% | 94.22 ± 0.37% | 171 ms | **2.12 s** | 2.29 s | **27.6** |
| **prefixaware**  | 43.75 ± 3.65% | **94.88 ± 0.08%** | **166 ms** | 2.13 s | **2.25 s** | 26.8 |

## What the data shows

1. **PA's KV cache rate is the rock-solid 94% across ALL concurrency at BOTH model sizes.** Other strategies climb from 80% → ~94% as load grows (incidental hits; the strongest baseline tops out at 94.60% on 7B and 94.22% on 14B). Only PA holds 94% from sessions=4 to sessions=24. The router is making correct decisions regardless of load.

2. **PA's TTFT slope is the gentlest** (see headline table above). At 14B, PA grows by 25 ms across 6× concurrency increase; random grows by 98 ms. **PA's predictability under load is its production value.**

3. **PA's per-point p50 wins start at sessions=16** (four sessions per worker; the crossover sits between sessions=12 and sessions=16). At sessions=16: PA beats round-robin on p50 by 8% (7B) and 13% (14B). At sessions=24: 14% over round-robin at both model sizes, 32-37% over random.

4. **Same shape at 7B and 14B** proves the algorithm, not the model.

5. **Honest failure mode**: at sessions=4, `leastloaded` beats PA by 17-19 ms (LL accidentally distributes one session per worker; intra-session multi-turn naturally pins via cache). PA's pinning adds router overhead with no marginal benefit at this regime.

## Caveats and limitations

- **Synthetic trace, not production capture.** Multi-turn sessions with a shared system prompt match the real shape of agentic workloads (Cursor, Claude Code, ReAct loops) but the prompt content is generated, not observed. Real workloads may have more diverse prefixes that fragment the cache.
- **Same model on every worker.** This bench does not cover multi-model deployments where different workers serve different models; routing is purely about cache locality, not model selection.
- **3 seeds is not a tight CI.** The error bars in the chart are ±1 sample stddev across 3 seeds; bootstrap CIs over the per-request distribution would be tighter (future work).
- **`max_tokens=64` keeps decode short.** Cache benefit is largest when prefill dominates total time. Real production workloads with longer responses (256-1024 tokens) would see relatively smaller PA improvements over baselines, though absolute improvements scale.
- **vLLM 0.6.4 specifically.** Newer vLLM versions (0.7+, 0.8+) have changed the prefix caching defaults; numbers may shift slightly. The relative comparison should hold.
- **One pod was preempted mid-bench** during the 14B run; results were SCP'd before re-running.

## Reproducing this exact data

See [cloud-bench.md](cloud-bench.md) for the full runbook. The short version:

```bash
# on a fresh RunPod 4× A100 80GB SXM pod, 50 GB container disk:
git clone https://github.com/zxuhan/llm-router.git
cd llm-router
bash scripts/install-cloud.sh                         # Go + vLLM + pinned deps
tmux new -s bench                                     # survives SSH disconnects

# 7B sweep, ~1.5 hr, ~$10
MODEL_DIR=models/qwen2.5-7b bash scripts/bench/concurrency-sweep.sh

# 14B sweep, ~2 hr, ~$13
rm -rf models/qwen2.5-7b   # free disk
MODEL_DIR=models/qwen2.5-14b bash scripts/bench/concurrency-sweep.sh
```

Or one shot for both: `bash scripts/bench/full-bench.sh`.

After SCP back to the laptop:

```bash
/tmp/plot-venv/bin/python scripts/plot/hero-cloud.py \
  --top    bench-results-sweep-7B/qwen2.5-7b   --top-title    "Qwen2.5-7B" \
  --bottom bench-results-sweep-14B/qwen2.5-14b --bottom-title "Qwen2.5-14B" \
  --out docs/images/hero-cloud.png
```
