package router

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zxuhan/llm-router/internal/backend"
)

// These microbenchmarks measure the cost of Router.Choose itself - i.e. the
// per-request overhead the proxy adds before any HTTP I/O. Run with:
//
//	go test -bench=. -benchmem -count=3 ./internal/router
//
// Reference numbers from `benchstat` are not committed because they vary
// per machine; the relative shapes (round-robin O(1), prefix-aware
// O(N) plus a tree walk) are stable across machines.

// makeBenchBackends returns n fake backends with realistic-looking IDs.
func makeBenchBackends(n int) []backend.Backend {
	out := make([]backend.Backend, n)
	for i := 0; i < n; i++ {
		out[i] = newFakeBackend(fmt.Sprintf("w%d", i))
	}
	return out
}

func benchChoose(b *testing.B, r Router, prompt string) {
	b.ReportAllocs()
	b.ResetTimer()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		if _, err := r.Choose(ctx, prompt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRoundRobin_Choose(b *testing.B) {
	for _, n := range []int{2, 4, 16} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			r := NewRoundRobin(makeBenchBackends(n))
			benchChoose(b, r, "")
		})
	}
}

func BenchmarkRandom_Choose(b *testing.B) {
	for _, n := range []int{2, 4, 16} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			r := NewRandom(makeBenchBackends(n))
			benchChoose(b, r, "")
		})
	}
}

func BenchmarkLeastLoaded_Choose(b *testing.B) {
	for _, n := range []int{2, 4, 16} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			r := NewLeastLoaded(makeBenchBackends(n))
			benchChoose(b, r, "")
		})
	}
}

func BenchmarkPrefixAware_Choose(b *testing.B) {
	prompts := map[string]string{
		"short":  strings.Repeat("hello ", 10),         // ~60 bytes
		"medium": strings.Repeat("system prompt ", 50), // ~700 bytes
		"long":   strings.Repeat("context ", 500),      // ~4 KB
	}
	for _, n := range []int{2, 4, 16} {
		for promptName, prompt := range prompts {
			b.Run(fmt.Sprintf("n=%d/%s", n, promptName), func(b *testing.B) {
				backends := makeBenchBackends(n)
				r := NewPrefixAware(backends, PrefixAwareOptions{
					Chunker:        NewChunker(32),
					MinMatchChunks: 1,
				})
				// Pre-populate every worker's tree so we exercise the
				// match path, not the cold path. Each backend gets a
				// unique tail so we get realistic divergence.
				for i, bk := range backends {
					r.Update(prompt+fmt.Sprintf(" tail-%d", i), bk)
				}
				benchChoose(b, r, prompt)
			})
		}
	}
}

// BenchmarkPrefixAware_Update measures the cost of recording a prompt
// after dispatch. Real workloads call this once per request.
func BenchmarkPrefixAware_Update(b *testing.B) {
	backends := makeBenchBackends(4)
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:        NewChunker(32),
		MinMatchChunks: 1,
	})
	prompt := strings.Repeat("system prompt ", 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Vary the suffix so the tree actually grows; otherwise we are
		// measuring "touch existing terminal" only.
		r.Update(prompt+fmt.Sprintf(" %d", i), backends[i%len(backends)])
	}
}
