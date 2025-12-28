# ADR 0004: LRU eviction with chunk-budget, prune dead branches, no rebalancing

## Context

The router's tree predicts a worker's KV-cache state. Real KV caches are
finite, and the actual hardware evicts older entries on pressure. Our tree
must track that approximately or it will give increasingly stale advice.

The goal: at all times, a tree's chunk count is bounded by the worker's
configured `kv_budget`, and the chunks present in the tree are the ones the
worker most likely still has cached.

## Decision

- Keep an LRU list of **terminal** nodes. A terminal node corresponds to a
  prompt that has actually been dispatched.
- On `Insert`, mark the destination node terminal (or move it to the front
  of the LRU if it was already terminal). After insert, while the tree's
  total chunk count exceeds `MaxChunks`, evict the oldest terminal:
  - Clear its terminal flag and remove it from the LRU.
  - Walk up the parent chain. As long as the current node has no children
    and is not terminal, remove it from its parent and reclaim its edge
    chunks. Stop at the root or at the first ancestor that is still
    needed.
- Do **not** re-merge a parent with a single remaining child after pruning.

## Rationale

- LRU is what real KV caches approximately do; matching the same policy
  keeps the prediction accurate.
- Tracking only terminals (not internal nodes) keeps the LRU short and
  meaningful: an internal node is always either still on someone else's
  path or already removable.
- Skipping the "merge parent with sole child" step keeps the eviction code
  simple and provably correct without recursion. The cost is at most one
  extra hop per query in the common case where a sibling has been evicted.
  This is a constant-factor concern; we have not measured it materially in
  benchmarks.
- The prune walk is bounded by tree depth, which is `O(prompt_length /
  chunk_size)`. With 32-byte chunks and ~16 KB prompts, that is ~500 hops
  in the worst case, all under a single mutex.

## Consequences

- Two prompts that share a prefix and one of which is evicted leave the
  shared prefix in the tree (the surviving prompt still pins it). The
  evicted prompt's unique tail is reclaimed.
- The chunk budget is a *soft* upper bound: the tree may briefly hold more
  than the budget mid-insert, but `evictUntilFits` runs synchronously
  before `Insert` returns, so by the time the call completes, the budget
  is honoured.
- Stats include an `Evicted` counter so the user can see when budgets are
  too tight in practice.
