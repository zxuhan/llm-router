package router

import (
	"context"
	"sort"

	"github.com/zxuhan/llm-router/internal/backend"
	"github.com/zxuhan/llm-router/internal/prefixtree"
)

// PrefixAware is the headline strategy: each backend has its own radix tree
// of recently-dispatched prompt prefixes. On each request the router asks
// every tree for the longest matching leading-chunk run and pins to the
// worker with the longest match, falling back to least-loaded when no worker
// has at least MinMatchChunks shared chunks.
//
// PrefixAware also implements a safety valve: if the worker with the longest
// match is at or above SaturationInflight outstanding requests, the router
// spills to the next-best worker whose match still meets MinMatchChunks and
// is below saturation, falling back to LeastLoaded only when no suitable
// alternative exists.
type PrefixAware struct {
	base
	chunker            Chunker
	minMatchChunks     int
	saturationInflight int
	fallback           Router
	trees              map[string]*prefixtree.Tree
}

// PrefixAwareOptions configures a PrefixAware router.
type PrefixAwareOptions struct {
	// Chunker hashes a prompt into chunks. Defaults to NewChunker(32) if nil.
	Chunker Chunker
	// MinMatchChunks is the minimum number of matching chunks before pinning
	// to a particular worker. Below this, the router falls back to the
	// configured Fallback (default: LeastLoaded over the same backends).
	MinMatchChunks int
	// SaturationInflight is the in-flight count at or above which a worker is
	// considered saturated. The safety valve then spills to the next-best
	// match. A value of zero disables the safety valve (always pin to best).
	SaturationInflight int
	// Fallback is the router consulted when no worker meets the match
	// threshold or all best-prefix workers are saturated. Defaults to
	// NewLeastLoaded(backends).
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
		base:               newBase(backends),
		chunker:            chunker,
		minMatchChunks:     opts.MinMatchChunks,
		saturationInflight: opts.SaturationInflight,
		fallback:           fb,
		trees:              trees,
	}
}

// Name implements Router.
func (*PrefixAware) Name() string { return "prefixaware" }

// candidate captures one backend's view of the routing decision: how much of
// the prompt it already holds and how loaded it currently is.
type candidate struct {
	idx      int
	match    int
	inflight int64
}

// Choose implements Router.
func (p *PrefixAware) Choose(ctx context.Context, prompt string) (Decision, error) {
	pool := p.healthy()
	if len(pool) == 0 {
		return Decision{}, ErrNoBackends
	}
	chunks := p.chunker(prompt)

	// Snapshot every healthy backend's match length and current inflight.
	cands := make([]candidate, len(pool))
	for i, b := range pool {
		cands[i] = candidate{
			idx:      i,
			match:    p.trees[b.ID()].LongestMatch(chunks),
			inflight: b.Inflight(),
		}
	}
	// Sort by (match desc, inflight asc, idx asc) so the result is stable.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].match != cands[j].match {
			return cands[i].match > cands[j].match
		}
		if cands[i].inflight != cands[j].inflight {
			return cands[i].inflight < cands[j].inflight
		}
		return cands[i].idx < cands[j].idx
	})

	best := cands[0]
	if best.match < p.minMatchChunks {
		// Threshold not met: defer to the fallback router.
		d, err := p.fallback.Choose(ctx, prompt)
		if err != nil {
			return Decision{}, err
		}
		d.Reason = "fallback-" + d.Reason
		d.MatchChunks = best.match
		return d, nil
	}

	// Safety valve: if the best worker is saturated, spill to the next-best
	// match that is also above threshold and not saturated.
	if p.saturationInflight > 0 && best.inflight >= int64(p.saturationInflight) {
		for _, c := range cands[1:] {
			if c.match < p.minMatchChunks {
				break // remaining candidates have even shorter match
			}
			if c.inflight < int64(p.saturationInflight) {
				return Decision{
					Backend:     pool[c.idx],
					MatchChunks: c.match,
					Reason:      "spilled-from-saturated",
				}, nil
			}
		}
		// No suitable alternative; spill to least-loaded.
		d, err := p.fallback.Choose(ctx, prompt)
		if err != nil {
			return Decision{}, err
		}
		d.Reason = "spilled-fallback-" + d.Reason
		d.MatchChunks = best.match
		return d, nil
	}

	return Decision{
		Backend:     pool[best.idx],
		MatchChunks: best.match,
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
