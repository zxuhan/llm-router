# ADR 0006: Hash fixed-size byte chunks instead of running a real tokenizer

Status: accepted, 2026-03

## Context

The prefix tree needs a deterministic mapping from prompt text to a sequence
of comparable units. Two natural approaches:

1. **Tokenize.** Run a tokenizer (BPE for GPT-style, SentencePiece for
   LLaMA-style) and feed token IDs to the tree. This matches what the KV
   cache actually keys on.
2. **Chunk and hash.** Slice the prompt into fixed-size byte windows and
   FNV-1a hash each window into a `uint64`.

## Decision

Take the chunk-and-hash approach. Default chunk size is 32 bytes. The
chunker is hot-pluggable via the `Chunker` type so callers can swap in a
tokenizer-backed chunker if they want.

## Rationale

- **Tokenizers are model-specific.** A router that fronts heterogeneous
  workers cannot assume one tokenizer. Even within a single model family,
  the BPE vocab can change between checkpoints.
- **Routing decisions only need a *monotone* function of the byte
  prefix.** If two prompts share their first N bytes, the chunker must
  produce the same first `floor(N/chunk_size)` chunk hashes. FNV-on-chunks
  has that property by construction. Whether the underlying tokenizer
  would have produced 7 or 9 tokens for the shared prefix doesn't matter
  for "this worker probably has it".
- **Zero dependencies.** No tokenizer.json, no model-specific assets.
- **Cheap.** FNV-1a runs at GB/s on a single core. The hash is dwarfed by
  the cost of the request itself.
- **Trade-off is bounded.** For chunk size 32 bytes and English text
  averaging ~4 bytes/token, one chunk is ~8 tokens. Two prompts that
  diverge inside the same chunk count as a divergence at chunk granularity,
  not at token granularity. The worst-case drift between the chunker's
  match and the actual KV-cache hit is bounded by chunk_size bytes (~8
  tokens), which is small relative to typical shared system prompts of
  hundreds of tokens.

## Consequences

- A small `Chunker` interface lets us swap in a tokenizer-backed chunker
  in production deployments where the operator knows the model.
- Diagnostics report tree sizes in chunks, not tokens. The `chunk_size`
  config parameter is the only knob the operator needs.
- The fuzz tests cross-check the tree against a brute-force oracle, so any
  pathological input the FNV-1a chunker produces is covered.

## Alternatives considered

- **MurmurHash, xxHash.** Either would also work. FNV-1a was chosen because
  it is in the Go standard library and the difference for our chunk sizes
  is irrelevant.
- **Variable-size chunks via content-defined chunking (rsync-style).**
  Tempting because it would maintain alignment after small in-the-middle
  edits, but it changes the cost model in ways that need their own ADR.
  The simple fixed-size chunker is enough for the prefix-aware case the
  project targets.
