package router

import "hash/fnv"

// Chunker splits a prompt into a sequence of uint64 chunk hashes for the
// prefix tree. The exact algorithm is intentionally simple: take fixed-size
// byte windows of the prompt and FNV-1a hash each window.
//
// We hash bytes, not runes. UTF-8 boundary alignment does not affect hash
// equality across two prompts that share a byte-identical prefix, which is
// the only invariant the prefix tree depends on.
type Chunker func(prompt string) []uint64

// NewChunker returns a Chunker that uses chunkSize-byte windows. A non-positive
// chunkSize falls back to 32 bytes.
//
// Trade-offs: smaller chunks mean finer-grained matches and a deeper tree
// (more memory, more pointer chasing); larger chunks mean coarser matches but
// less overhead. The default of 32 bytes was chosen as a rough analogue of
// 8 tokens, given an average of ~4 bytes per token in English-leaning text.
// See docs/decisions/0006-tokenization-strategy.md.
func NewChunker(chunkSize int) Chunker {
	if chunkSize <= 0 {
		chunkSize = 32
	}
	return func(prompt string) []uint64 {
		if prompt == "" {
			return nil
		}
		n := len(prompt)
		out := make([]uint64, 0, (n+chunkSize-1)/chunkSize)
		for i := 0; i < n; i += chunkSize {
			end := i + chunkSize
			if end > n {
				end = n
			}
			h := fnv.New64a()
			_, _ = h.Write([]byte(prompt[i:end]))
			out = append(out, h.Sum64())
		}
		return out
	}
}
