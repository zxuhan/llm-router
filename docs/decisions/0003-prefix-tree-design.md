# ADR 0003: Compressed radix tree, one per worker, behind RWMutex

## Context

The router needs an index of "what each worker probably already has in its
KV cache" that supports two operations cheaply:

- `LongestMatch(prompt) -> int chunks`: how many leading chunks of this
  prompt does this worker already hold? Called once per backend per
  request.
- `Insert(prompt)`: record that this worker now holds these chunks. Called
  once per request, after a routing decision.

The index needs to be safe under high concurrency (one HTTP server with
hundreds of in-flight requests).

## Decision

- Use a **compressed radix tree** keyed on sequences of `uint64` chunk
  hashes.
- Maintain **one tree per worker** rather than a global tree with
  worker-set bitmaps on each node.
- Protect each tree with a `sync.RWMutex`. Reads (`LongestMatch`) take
  `RLock`; writes (`Insert`, eviction) take `Lock`.

## Rationale

- A radix tree compresses common prefixes by definition. Two prompts that
  share the first 2 KB of system instructions plus diverge for the last
  100 chars allocate one shared edge for the prefix and two short edges for
  the suffixes. Memory grows roughly with the *unique* content across
  prompts.
- Per-worker trees mirror the per-worker KV cache. A global tree with
  per-node worker sets would be slightly more memory-efficient when many
  workers share the same prefixes, but the locking story becomes more
  involved (per-node locks, or one big lock covering all workers) and the
  resulting code is harder to reason about.
- `RWMutex` is the right granularity here. The hot path is many concurrent
  `LongestMatch` calls; insertions are at the rate of dispatched requests
  (orders of magnitude lower). Per-node locks were considered but rejected
  for added complexity with no observed contention in the benchmark.

## Consequences

- The tree is allocated lazily: nodes are created on insert and freed when
  pruning empties a branch.
- The diagnostic counters (`inserts`, `queries`, `evicted`) are
  `atomic.Uint64` so a `LongestMatch` call can bump them under a read
  lock without contention.
- For a 70 B model with 32 K context per worker, even a worst-case fanout
  (every prompt unique past the first chunk) keeps the tree at hundreds of
  KB. We have not needed a more compact representation.

## Alternatives considered

- **Flat hash table of (worker, hash-of-first-N-bytes) pairs.** Faster
  lookups but cannot answer "how *much* do you have?", only yes/no for a
  fixed N.
- **Bloom filter per worker.** Very compact, but produces false positives
  and again only answers a yes/no.
- **Global radix tree with worker bitmaps.** See above.
