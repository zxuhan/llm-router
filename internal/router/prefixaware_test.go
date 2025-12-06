package router

import (
	"context"
	"strings"
	"testing"

	"github.com/xzhou/llm-router/internal/backend"
)

func TestPrefixAware_PinsToWorkerWithLongestMatch(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:        NewChunker(8),
		MinMatchChunks: 1,
	})
	if r.Name() != "prefixaware" {
		t.Errorf("Name = %q", r.Name())
	}

	// Step 1: Update tells "a" it has dispatched a long shared-prefix prompt.
	prompt := "system: be helpful and concise.\nuser: hello"
	r.Update(prompt, backends[0])

	// Step 2: A new prompt with the same prefix arrives. PrefixAware should
	// route it to a (the only worker that has matching chunks).
	d, err := r.Choose(context.Background(), prompt+" again")
	if err != nil {
		t.Fatal(err)
	}
	if d.Backend.ID() != "a" {
		t.Errorf("got %q, want a", d.Backend.ID())
	}
	if d.MatchChunks <= 0 {
		t.Errorf("MatchChunks = %d, want > 0", d.MatchChunks)
	}
	if d.Reason != "longest-prefix" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestPrefixAware_FallsBackWhenNoMatchAboveThreshold(t *testing.T) {
	backends := makeStubs("a", "b")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:        NewChunker(8),
		MinMatchChunks: 4,
	})
	// Train "a" with a short prompt; the new query won't share enough.
	r.Update("hi there", backends[0])
	asFake(backends[0]).SetInflight(5) // make a heavily loaded
	asFake(backends[1]).SetInflight(0) // b is idle

	// Query something completely different.
	d, err := r.Choose(context.Background(), "totally unrelated content here")
	if err != nil {
		t.Fatal(err)
	}
	// Fallback path is least-loaded -> b.
	if d.Backend.ID() != "b" {
		t.Errorf("got %q, want b (fallback to least-loaded)", d.Backend.ID())
	}
	if !strings.HasPrefix(d.Reason, "fallback-") {
		t.Errorf("Reason = %q, want fallback-* prefix", d.Reason)
	}
}

func TestPrefixAware_NoBackends(t *testing.T) {
	r := NewPrefixAware(nil, PrefixAwareOptions{})
	if _, err := r.Choose(context.Background(), "x"); err == nil {
		t.Fatal("expected ErrNoBackends")
	}
}

func TestPrefixAware_DefaultsAreUsable(t *testing.T) {
	backends := makeStubs("a", "b")
	// All-default options.
	r := NewPrefixAware(backends, PrefixAwareOptions{})
	d, err := r.Choose(context.Background(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	// With no inserts, no worker matches; threshold 0 means the longest-match
	// branch wins anyway and pins to backends[0] deterministically.
	if d.Backend.ID() != "a" {
		t.Errorf("got %q, want a", d.Backend.ID())
	}
}

func TestPrefixAware_UpdateSegregatesPerBackend(t *testing.T) {
	backends := makeStubs("a", "b")
	r := NewPrefixAware(backends, PrefixAwareOptions{
		Chunker:        NewChunker(4),
		MinMatchChunks: 1,
	})
	// Insert two distinct prompts on two distinct backends.
	r.Update("AAAAAAAAAA", backends[0])
	r.Update("BBBBBBBBBB", backends[1])

	if d, _ := r.Choose(context.Background(), "AAAAAAAA-extra"); d.Backend.ID() != "a" {
		t.Errorf("A-prompt routed to %q, want a", d.Backend.ID())
	}
	if d, _ := r.Choose(context.Background(), "BBBBBBBB-extra"); d.Backend.ID() != "b" {
		t.Errorf("B-prompt routed to %q, want b", d.Backend.ID())
	}
}

func TestPrefixAware_UpdateUnknownBackendIgnored(t *testing.T) {
	backends := makeStubs("a")
	r := NewPrefixAware(backends, PrefixAwareOptions{Chunker: NewChunker(4)})
	other := newFakeBackend("ghost")
	// Must not panic, must not mutate trees.
	r.Update("hello", other)
	st := r.TreeStats()["a"]
	if st.Inserts != 0 {
		t.Errorf("a's tree should be untouched; inserts=%d", st.Inserts)
	}
}

func TestPrefixAware_TreeStatsMatchInserts(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	r := NewPrefixAware(backends, PrefixAwareOptions{Chunker: NewChunker(8)})
	r.Update("first", backends[0])
	r.Update("second", backends[0])
	r.Update("alpha", backends[2])

	stats := r.TreeStats()
	if stats["a"].Inserts != 2 {
		t.Errorf("a inserts = %d", stats["a"].Inserts)
	}
	if stats["b"].Inserts != 0 {
		t.Errorf("b inserts = %d", stats["b"].Inserts)
	}
	if stats["c"].Inserts != 1 {
		t.Errorf("c inserts = %d", stats["c"].Inserts)
	}
}

func TestPrefixAware_FallbackErrorIsPropagated(t *testing.T) {
	// A fallback that always errors lets us cover the error-propagation path.
	bad := errFallback{}
	r := NewPrefixAware(makeStubs("a"), PrefixAwareOptions{
		Chunker:        NewChunker(4),
		MinMatchChunks: 1,
		Fallback:       bad,
	})
	if _, err := r.Choose(context.Background(), "x"); err == nil {
		t.Fatal("expected propagated fallback error")
	}
}

// errFallback is a Router that always returns ErrNoBackends.
type errFallback struct{}

func (errFallback) Name() string                                     { return "err" }
func (errFallback) Choose(context.Context, string) (Decision, error) { return Decision{}, ErrNoBackends }
func (errFallback) Update(string, backend.Backend)                   {}

