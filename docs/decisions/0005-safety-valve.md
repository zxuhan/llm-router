# ADR 0005: Safety valve for saturated workers

## Context

Pinning every request that shares a prefix to the same worker is fine until
that worker is saturated. The KV-cache hit was supposed to *save* time, but
if the worker has 100 in-flight requests already, head-of-line blocking
will eat any gain.

## Decision

The `PrefixAware` strategy includes a configurable `SaturationInflight`
threshold. The decision flow is:

1. Compute every backend's `(match_chunks, inflight)` snapshot.
2. Sort by `(match desc, inflight asc, idx asc)`.
3. If the best worker's `match < MinMatchChunks`, fall back to the
   configured fallback router (default `LeastLoaded`). Tag the decision
   `fallback-<inner-reason>`.
4. Otherwise, if `SaturationInflight > 0` and the best worker's
   `inflight >= SaturationInflight`:
   - Walk the sorted list looking for the next candidate whose `match >=
     MinMatchChunks` and `inflight < SaturationInflight`. If found, route
     there with reason `spilled-from-saturated`.
   - If none found, fall back to the configured fallback. Tag the decision
     `spilled-fallback-<inner-reason>`.
5. Otherwise route to the best. Tag the decision `longest-prefix`.

## Rationale

- The valve trades **maximum cache hit rate** for **lower tail latency
  under load**. Without a valve, all traffic for a hot prompt funnels to
  one worker; with a valve, slightly stale tail traffic spreads.
- The threshold is a tunable, not an algorithmic constant, because the
  right value depends on (a) per-request decode time and (b) the worker's
  internal request queueing. A llama.cpp server with `--parallel 4` and a
  fast decoder is happy at `inflight=8`; a slower model is not.
- Distinct `Reason` strings are surfaced in metrics labels and the
  `X-Router-Reason` response header so the operator can see how often the
  valve fires without setting up a custom counter.

## Consequences

- The router now reads `Backend.Inflight()` on every decision. That is one
  atomic load per backend per request: trivial.
- Tests in `internal/router/safetyvalve_test.go` cover all four landing
  states (best-pin, spill-to-next, spill-to-fallback, threshold-fallback).
- Operators are expected to observe the `reason` label distribution and
  adjust `saturation_inflight` if they see runaway spills (threshold too
  low) or queueing on the favoured worker (threshold too high).

## Alternatives considered

- **No valve, trust LRU.** Catastrophic under bursty traffic.
- **Valve based on a hot-path queue depth metric instead of in-flight.**
  Requires the upstream to expose it; we want the simplest signal that is
  always available.
- **Valve via a second-best random fallback.** Loses determinism for very
  little benefit; tie-breaking on (match, inflight, idx) is already stable
  and deterministic.
