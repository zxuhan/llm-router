package router

import (
	"strings"
	"testing"
)

func TestChunker_DefaultsWhenChunkSizeNonPositive(t *testing.T) {
	c0 := NewChunker(0)
	cNeg := NewChunker(-5)
	prompt := strings.Repeat("x", 100)
	if len(c0(prompt)) != len(cNeg(prompt)) {
		t.Errorf("zero and negative should default to the same chunk size")
	}
	// 100 bytes / 32 = 4 chunks (3 full + 1 partial).
	if got := len(c0(prompt)); got != 4 {
		t.Errorf("len = %d, want 4", got)
	}
}

func TestChunker_DeterministicAcrossCalls(t *testing.T) {
	c := NewChunker(8)
	p := "the quick brown fox jumps over the lazy dog"
	a := c(p)
	b := c(p)
	if len(a) != len(b) {
		t.Fatalf("len mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("chunk %d differs: %d vs %d", i, a[i], b[i])
		}
	}
}

func TestChunker_PrefixesShareLeadingChunks(t *testing.T) {
	c := NewChunker(8)
	a := c("system prompt: be helpful and concise. user: hi")
	b := c("system prompt: be helpful and concise. user: bye")
	// Up through "user: ", the bytes are identical, so the leading 8-byte
	// chunks must be identical too.
	shared := 0
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			break
		}
		shared++
	}
	if shared < 5 {
		t.Errorf("expected >=5 shared leading chunks, got %d", shared)
	}
}

func TestChunker_EmptyPrompt(t *testing.T) {
	c := NewChunker(32)
	if got := c(""); got != nil {
		t.Errorf("empty chunker output = %v, want nil", got)
	}
}

func TestChunker_SmallerThanChunkSize(t *testing.T) {
	c := NewChunker(32)
	got := c("hi")
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
}
