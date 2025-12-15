# Architecture Decision Records

Each file documents a load-bearing design choice and the reasoning behind
it. Keep ADRs short, focused, and immutable: when a decision is reversed,
add a new ADR that supersedes the old one rather than editing in place.

| ID | Title | Status |
| --- | --- | --- |
| 0001 | [Implement the router in Go (rather than Rust)](0001-language-choice.md) | accepted |
| 0002 | [Default to llama.cpp's HTTP server as the upstream backend](0002-backend-choice.md) | accepted |
| 0003 | [Compressed radix tree, one per worker, behind RWMutex](0003-prefix-tree-design.md) | accepted |
| 0004 | [LRU eviction with chunk-budget, prune dead branches](0004-eviction-policy.md) | accepted |
| 0005 | [Safety valve for saturated workers](0005-safety-valve.md) | accepted |
| 0006 | [Hash fixed-size byte chunks instead of running a real tokenizer](0006-tokenization-strategy.md) | accepted |
| 0007 | [Two-state circuit breaker per backend](0007-circuit-breaker.md) | accepted |
