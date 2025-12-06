package router

import (
	"context"

	"github.com/xzhou/llm-router/internal/backend"
	"github.com/xzhou/llm-router/internal/prefixtree"
)

// PrefixAware is the headline strategy: each backend has its own radix tree
// of recently-dispatched prompt prefixes. On each request the router asks
// every tree for the longest matching leading-chunk run and pins to the
// worker with the longest match, falling back to least-loaded when no worker
// has at least MinMatchChunks shared chunks.
//
// The safety valve (spill on saturation) is layered on top in a follow-up
// change; see WithSafetyValve.
type PrefixAware struct {
	base
	chunker        Chunker
	minMatchChunks int
	fallback       Router
	trees          map[string]*prefixtree.Tree
}

// PrefixAwareOptions configures a PrefixAware router.
type PrefixAwareOptions struct {
	// Chunker hashes a prompt into chunks. Defaults to NewChunker(32) if nil.
	Chunker Chunker
	// MinMatchChunks is the minimum number of matching chunks before pinning
	// to a particular worker. Below this, the router falls back to the
	// configured Fallback (default: LeastLoaded over the same backends).
	MinMatchChunks int
	// Fallback is the router consulted when no worker meets the match
	// threshold. Defaults to NewLeastLoaded(backends) if nil.
	Fallback Router
}

// NewPrefixAware constructs a PrefixAware router. Each backend gets a tree
// sized by its KVBudget (chunks). Backends with a zero KVBudget get an
// unbounded tree.
func NewPrefixAware(backends []backend.Backend, opts PrefixAwareOptions) *PrefixAware {
	chunker := opts.Chunker
	if chunker == nil {
		chunker = NewChunker(32)
	}
	fb := opts.Fallback
	if fb == nil {
		fb = NewLeastLoaded(backends)
	}
	trees := make(map[string]*prefixtree.Tree, len(backends))
	for _, b := range backends {
		trees[b.ID()] = prefixtree.NewTree(b.KVBudget())
	}
	return &PrefixAware{
		base:           newBase(backends),
		chunker:        chunker,
		minMatchChunks: opts.MinMatchChunks,
		fallback:       fb,
		trees:          trees,
	}
}

// Name implements Router.
func (*PrefixAware) Name() string { return "prefixaware" }

// Choose implements Router.
func (p *PrefixAware) Choose(ctx context.Context, prompt string) (Decision, error) {
	if len(p.backends) == 0 {
		return Decision{}, ErrNoBackends
	}
	chunks := p.chunker(prompt)

	// Walk every backend's tree and remember the longest match.
	bestIdx := 0
	bestMatch := p.trees[p.backends[0].ID()].LongestMatch(chunks)
	for i := 1; i < len(p.backends); i++ {
		m := p.trees[p.backends[i].ID()].LongestMatch(chunks)
		if m > bestMatch {
			bestMatch = m
			bestIdx = i
		}
	}

	if bestMatch < p.minMatchChunks {
		d, err := p.fallback.Choose(ctx, prompt)
		if err != nil {
			return Decision{}, err
		}
		// Tag the decision so observers can see why we landed here.
		d.Reason = "fallback-" + d.Reason
		d.MatchChunks = bestMatch
		return d, nil
	}

	return Decision{
		Backend:     p.backends[bestIdx],
		MatchChunks: bestMatch,
		Reason:      "longest-prefix",
	}, nil
}

// Update implements Router.
func (p *PrefixAware) Update(prompt string, chosen backend.Backend) {
	t, ok := p.trees[chosen.ID()]
	if !ok {
		return
	}
	t.Insert(p.chunker(prompt))
}

// TreeStats returns a snapshot of every backend's prefix tree statistics.
// Useful for /metrics and for tests.
func (p *PrefixAware) TreeStats() map[string]prefixtree.Stats {
	out := make(map[string]prefixtree.Stats, len(p.trees))
	for id, t := range p.trees {
		out[id] = t.Stats()
	}
	return out
}
