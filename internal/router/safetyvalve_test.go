package router

import (
	"context"
	"strings"
	"testing"
)

// All tests in this file exercise the SaturationInflight ("safety valve") code
// path in PrefixAware. The goal is to keep best-prefix routing when the chosen
// worker can serve, and to gracefully spill to the next-best match (or the
// fallback) only when the chosen worker is overloaded.

func TestSafetyValve_NotEngagedWhenBelowThreshold(t *testing.T) {
	backends := makeStubs("a", "b")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:            NewChunker(4),
		MinMatchChunks:     1,
		SaturationInflight: 4,
	})
	r.Update("AAAAAAAA", backends[0])
	asFake(backends[0]).SetInflight(2) // below threshold

	d, _ := r.Choose(context.Background(), "AAAAAAAA")
	if d.Backend.ID() != "a" {
		t.Errorf("expected pin to a, got %q", d.Backend.ID())
	}
	if d.Reason != "longest-prefix" {
		t.Errorf("Reason = %q, want longest-prefix", d.Reason)
	}
}

func TestSafetyValve_SpillsToNextBestMatch(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:            NewChunker(4),
		MinMatchChunks:     1,
		SaturationInflight: 4,
	})
	// "a" has the longest match, "b" has a shorter (but above-threshold) match.
	r.Update("AAAAAAAAAA", backends[0]) // longest-prefix candidate
	r.Update("AAAA", backends[1])       // shorter match

	asFake(backends[0]).SetInflight(8) // saturated
	asFake(backends[1]).SetInflight(0) // available

	d, _ := r.Choose(context.Background(), "AAAAAAAA")
	if d.Backend.ID() != "b" {
		t.Errorf("expected spill to b (next best), got %q", d.Backend.ID())
	}
	if d.Reason != "spilled-from-saturated" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestSafetyValve_SpillsToFallbackWhenNoAlternative(t *testing.T) {
	backends := makeStubs("a", "b")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:            NewChunker(4),
		MinMatchChunks:     1,
		SaturationInflight: 4,
	})
	// Only "a" has a match. Saturate it.
	r.Update("AAAAAAAA", backends[0])
	asFake(backends[0]).SetInflight(8)
	asFake(backends[1]).SetInflight(2)

	d, _ := r.Choose(context.Background(), "AAAAAAAA")
	if d.Backend.ID() != "b" {
		t.Errorf("expected spill to fallback (b is least-loaded), got %q", d.Backend.ID())
	}
	if !strings.HasPrefix(d.Reason, "spilled-fallback-") {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestSafetyValve_ZeroDisablesSpill(t *testing.T) {
	backends := makeStubs("a", "b")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:            NewChunker(4),
		MinMatchChunks:     1,
		SaturationInflight: 0, // disabled
	})
	r.Update("AAAAAAAA", backends[0])
	asFake(backends[0]).SetInflight(100) // doesn't matter
	asFake(backends[1]).SetInflight(0)

	d, _ := r.Choose(context.Background(), "AAAAAAAA")
	if d.Backend.ID() != "a" {
		t.Errorf("with safety valve disabled, expected a; got %q", d.Backend.ID())
	}
	if d.Reason != "longest-prefix" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestSafetyValve_AllSaturatedSpillsToFallbackError(t *testing.T) {
	backends := makeStubs("a", "b")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:            NewChunker(4),
		MinMatchChunks:     1,
		SaturationInflight: 4,
		Fallback:           errFallback{}, // forces error path
	})
	r.Update("AAAAAAAA", backends[0])
	asFake(backends[0]).SetInflight(8)
	asFake(backends[1]).SetInflight(8)

	if _, err := r.Choose(context.Background(), "AAAAAAAA"); err == nil {
		t.Fatal("expected propagated error from fallback")
	}
}

func TestSafetyValve_SpillRespectsMinMatchThreshold(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:            NewChunker(4),
		MinMatchChunks:     3,
		SaturationInflight: 4,
	})
	// "a" has long match (>=3 chunks); "b" has only 1 chunk; "c" idle but 0 match.
	r.Update("AAAAAAAAAAAAAAAA", backends[0])
	r.Update("AAAA", backends[1])

	asFake(backends[0]).SetInflight(8) // saturated
	asFake(backends[1]).SetInflight(0)
	asFake(backends[2]).SetInflight(0)

	d, _ := r.Choose(context.Background(), "AAAAAAAAAAAAAAAA")
	// b's match is 1 chunk (below threshold 3); c has zero match. We must
	// spill all the way to least-loaded fallback.
	if !strings.HasPrefix(d.Reason, "spilled-fallback-") {
		t.Errorf("Reason = %q, want spilled-fallback-*", d.Reason)
	}
}
