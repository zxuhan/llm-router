// Package prefixtree implements a concurrent-safe compressed radix tree over
// sequences of chunk hashes. It is the data structure the prefix-aware router
// uses, per worker, to estimate which worker is most likely to already hold a
// given request's prefix in its KV cache.
//
// Concepts:
//
//   - A "chunk" is a fixed-size slice of the prompt text, hashed to a uint64.
//     The router decides chunk size and hashing; this package only sees uint64
//     sequences.
//   - Insert records a prompt: the path from root down to a terminal node
//     represents one prompt that has been dispatched to this worker.
//   - LongestMatch reports how many leading chunks of a query sequence are
//     already represented in the tree.
//
// The chunk-budget / LRU eviction layer is added separately; see lru.go.
package prefixtree

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// Tree is a concurrent-safe radix tree.
type Tree struct {
	mu sync.RWMutex

	root *node

	chunks    int // total chunks across all edges; protected by mu
	maxChunks int // 0 disables eviction

	// LRU of terminal nodes; front = newest, back = oldest. Protected by mu.
	lru *list.List

	// Aggregate counters for diagnostics; not used in routing decisions.
	// Atomics so LongestMatch can update queries while holding only RLock.
	inserts atomic.Uint64
	queries atomic.Uint64
	evicted atomic.Uint64
}

type node struct {
	edge     []uint64
	children map[uint64]*node
	parent   *node
	terminal bool
	// lruElem points to the LRU list element holding this node, when terminal.
	lruElem *list.Element
}

// NewTree returns an empty tree with the given chunk budget. A budget of zero
// (or negative; values are clamped to zero) disables eviction.
func NewTree(maxChunks int) *Tree {
	if maxChunks < 0 {
		maxChunks = 0
	}
	return &Tree{
		root:      &node{children: map[uint64]*node{}},
		maxChunks: maxChunks,
		lru:       list.New(),
	}
}

// Stats describes a tree's current state. It is a snapshot.
type Stats struct {
	Chunks    int    // total chunks across all edges
	Terminals int    // number of distinct terminal nodes
	MaxChunks int    // configured budget (0 = unlimited)
	Inserts   uint64 // cumulative inserts
	Queries   uint64 // cumulative LongestMatch calls
	Evicted   uint64 // cumulative evicted terminals
}

// Stats returns a snapshot of the tree's counters.
func (t *Tree) Stats() Stats {
	t.mu.RLock()
	chunks, terms, max := t.chunks, t.lru.Len(), t.maxChunks
	t.mu.RUnlock()
	return Stats{
		Chunks:    chunks,
		Terminals: terms,
		MaxChunks: max,
		Inserts:   t.inserts.Load(),
		Queries:   t.queries.Load(),
		Evicted:   t.evicted.Load(),
	}
}

// Len returns the number of chunks currently stored across all edges.
func (t *Tree) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.chunks
}

// LongestMatch returns the number of leading chunks of seq that are already
// represented as a path in the tree. It does not mutate the tree; concurrent
// readers are safe.
func (t *Tree) LongestMatch(seq []uint64) int {
	if len(seq) == 0 {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	t.queries.Add(1)

	cur := t.root
	matched := 0
	for matched < len(seq) {
		child, ok := cur.children[seq[matched]]
		if !ok {
			break
		}
		common := commonPrefix(child.edge, seq[matched:])
		matched += common
		if common < len(child.edge) {
			break
		}
		cur = child
	}
	return matched
}

// Insert records seq as a prompt held by this worker. Existing internal nodes
// are reused; edges are split as needed.
func (t *Tree) Insert(seq []uint64) {
	if len(seq) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inserts.Add(1)

	cur := t.root
	i := 0
	for i < len(seq) {
		child, ok := cur.children[seq[i]]
		if !ok {
			n := &node{
				edge:     append([]uint64(nil), seq[i:]...),
				children: map[uint64]*node{},
				parent:   cur,
			}
			cur.children[seq[i]] = n
			t.chunks += len(n.edge)
			t.markTerminal(n)
			t.evictUntilFits()
			return
		}

		common := commonPrefix(child.edge, seq[i:])
		if common == len(child.edge) {
			cur = child
			i += common
			continue
		}

		// Partial match: split child's edge at `common`.
		split := &node{
			edge:     append([]uint64(nil), child.edge[:common]...),
			children: map[uint64]*node{},
			parent:   cur,
		}
		child.edge = append([]uint64(nil), child.edge[common:]...)
		child.parent = split
		split.children[child.edge[0]] = child
		cur.children[seq[i]] = split

		i += common
		if i == len(seq) {
			t.markTerminal(split)
			t.evictUntilFits()
			return
		}
		tail := append([]uint64(nil), seq[i:]...)
		sib := &node{
			edge:     tail,
			children: map[uint64]*node{},
			parent:   split,
		}
		split.children[tail[0]] = sib
		t.chunks += len(tail)
		t.markTerminal(sib)
		t.evictUntilFits()
		return
	}
	// Sequence fully matches an existing path.
	t.markTerminal(cur)
	t.evictUntilFits()
}

// commonPrefix returns the length of the longest shared prefix between two
// slices.
func commonPrefix(a, b []uint64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
