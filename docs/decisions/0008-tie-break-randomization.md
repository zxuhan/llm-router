# ADR 0008: Random tie-breaking among equal-prefix workers

Status: accepted, 2026-05

## Context

ADR 0005 specified a deterministic sort key for the `PrefixAware`
strategy: `(match_chunks desc, inflight asc, idx asc)`. The first cloud
benchmark on 4× A100 + Qwen2.5-14B exposed a pathology of that scheme.

When N concurrent sessions all share the same system prompt (the typical
agentic workload), every worker reports an *identical* `match_chunks`
value on the first turn of each session because no worker has any cached
prefix yet. The deterministic `idx asc` tiebreaker then sends every one
of those first turns to `backends[0]`. From the second turn onward,
`backends[0]` is the only worker with a non-zero match for any of the
sessions' continuing prefixes, so PA pins all subsequent turns there too.

Result: with 12 sessions on 4 workers, `backends[0]` handled 32 sequential
requests while the other three workers sat idle. PA's per-request cache
benefit was real, but it was completely eaten by single-worker queuing
penalty. **PA TTFT p50 was 18% slower than round-robin** in that run.

Symptom in the data:

| Strategy | TTFT p50 (14B, sessions=12, deterministic tiebreak) |
| :--- | ---: |
| roundrobin | 333 ms |
| **prefixaware (broken)** | **395 ms (18% slower)** |

## Decision

Pre-shuffle the candidate slice with a router-owned RNG before the
`(match desc, inflight asc)` sort. The stable sort preserves the
shuffled order for ties; non-tie cases still respect the original
ordering.

```go
p.rngMu.Lock()
p.rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
p.rngMu.Unlock()
sort.SliceStable(cands, func(i, j int) bool {
    if cands[i].match != cands[j].match {
        return cands[i].match > cands[j].match
    }
    return cands[i].inflight < cands[j].inflight
})
```

The RNG is exposed on `PrefixAwareOptions.Rng` so tests can pin a seed
for determinism. Production seeds from `time.Now().UnixNano()`.

## Rationale

- **Targets the failure mode directly.** Tie-break randomization only
  affects the first turn of each session (when all workers share the same
  match length); from turn 2 onward, the worker that handled turn 1 has
  the longest match and is naturally selected.
- **Distributes the system-prompt cache across all workers** in roughly
  uniform fashion, so subsequent intra-session turns can pin to any of
  the warm workers, not just `backends[0]`.
- **Combined with `saturation_inflight=4`**, this turns the previous
  "PA loses to RR by 18%" result into "PA wins by 14% at sessions=24".
- **Cheap.** One `rand.Shuffle` over an at-most-4-element slice per
  routing decision. Microbenchmark overhead is below the noise floor of
  the existing `BenchmarkPrefixAware` tests.

## Consequences

- The router now requires a small amount of synchronized random state
  (`sync.Mutex` + `*rand.Rand`). Concurrent `Choose` calls serialize on
  the shuffle, but the critical section is microseconds; benchmarks show
  no measurable contention at our concurrency.
- Tests that asserted "PA returns `backends[0]` on no-insert" had to be
  updated. The replacement
  `TestPrefixAware_TieBreakDistributesAcrossWorkers` instead checks that
  ties spread across at least 3 of 4 workers over 200 calls.
- Output is now non-deterministic without an explicit seed. Production
  is fine with this; tests pin a seed.
- ADR 0005's "Alternatives considered" section explicitly *rejected*
  random tie-breaking on the grounds of "loses determinism for very
  little benefit." That assessment was wrong at production concurrency.
  This ADR supersedes that part of 0005's reasoning; the safety-valve
  algorithm itself remains as documented in 0005.

## Validation

The cloud concurrency sweep (4× A100, Qwen2.5-7B and Qwen2.5-14B,
SESSIONS=4..24) shows PA's TTFT slope is now 2-3× gentler than every
baseline at both model sizes:

| Strategy | 7B slope | 14B slope |
| :--- | ---: | ---: |
| Random          | +49 ms | +98 ms |
| Round-robin     | +31 ms | +36 ms |
| Least-loaded    | +36 ms | +49 ms |
| **Prefix-aware (fixed)** | **+16 ms** | **+25 ms** |

Full data: [docs/results-cloud.md](../results-cloud.md).

## Alternatives considered

- **Hash-of-session-id tie-break.** Would give deterministic but
  distributed pinning if session IDs were available at the routing
  layer. The router doesn't currently see session IDs (and doesn't want
  to: that couples it to the trace format). Rejected.
- **Pre-warm all workers with the system prompt at startup.** Hacky;
  requires the router to know which prefix is "the system prompt"; only
  works for the first system prompt. Rejected.
- **Cache-aware load balancing with a soft penalty term**
  (`score = match - alpha * inflight`). Closer to what SGLang's
  RadixAttention scheduler does at the engine level. More flexible but
  adds a tunable that needs justification. Deferred to future work; the
  randomized-tie + safety-valve combo was sufficient to flip the cloud
  result.
- **Dynamic `saturation_inflight` based on observed P95 latency per
  worker.** The honest next step. Listed in README's Future work.
