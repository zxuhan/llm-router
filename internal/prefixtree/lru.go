package prefixtree

// markTerminal flags n as the end of a stored prompt and updates its LRU
// position to "newest". Caller must hold the write lock.
func (t *Tree) markTerminal(n *node) {
	if n.terminal && n.lruElem != nil {
		t.lru.MoveToFront(n.lruElem)
		return
	}
	n.terminal = true
	n.lruElem = t.lru.PushFront(n)
}

// evictUntilFits evicts oldest terminals until total chunks fit the budget.
// A budget of zero disables eviction. Caller must hold the write lock.
func (t *Tree) evictUntilFits() {
	if t.maxChunks <= 0 {
		return
	}
	for t.chunks > t.maxChunks && t.lru.Len() > 0 {
		t.evictOldest()
	}
}

// evictOldest removes the oldest terminal from the LRU, clears its terminal
// flag, and prunes any now-dead branches up the tree. Caller holds the write
// lock.
func (t *Tree) evictOldest() {
	elem := t.lru.Back()
	if elem == nil {
		return
	}
	n, _ := elem.Value.(*node)
	t.lru.Remove(elem)
	n.lruElem = nil
	n.terminal = false
	t.evicted.Add(1)

	t.pruneFrom(n)
}

// pruneFrom walks up from n removing nodes that are no longer needed - i.e.
// non-terminal leaves. Stops at the first ancestor that is still terminal or
// has another live child, or at the root. Caller holds the write lock.
//
// This implementation does not re-merge a parent with a single remaining
// child after pruning. The functional impact is just a slightly deeper tree;
// see docs/decisions/0004-eviction-policy.md for rationale.
func (t *Tree) pruneFrom(n *node) {
	for n != nil && n.parent != nil {
		if n.terminal || len(n.children) > 0 {
			return
		}
		parent := n.parent
		// Remove n from parent's children map.
		key := n.edge[0]
		delete(parent.children, key)
		t.chunks -= len(n.edge)
		// Help GC: drop references.
		n.parent = nil
		n.children = nil
		n.edge = nil
		n = parent
	}
}
