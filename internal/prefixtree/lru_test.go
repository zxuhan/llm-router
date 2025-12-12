package prefixtree

import (
	"sync"
	"testing"
)

func TestEviction_DisabledByZeroBudget(t *testing.T) {
	tr := NewTree(0)
	for i := 0; i < 1000; i++ {
		tr.Insert(h(i, i+1, i+2))
	}
	if tr.Len() < 100 {
		t.Errorf("with budget 0, tree should grow; len = %d", tr.Len())
	}
	if got := tr.Stats().Evicted; got != 0 {
		t.Errorf("Evicted = %d, want 0", got)
	}
}

func TestEviction_NegativeBudgetClampsToZero(t *testing.T) {
	tr := NewTree(-5)
	tr.Insert(h(1, 2, 3))
	if tr.Len() != 3 {
		t.Errorf("Len = %d, want 3", tr.Len())
	}
}

func TestEviction_SinglePromptOverBudget(t *testing.T) {
	// Budget of 4; insert a 6-chunk prompt; the new prompt cannot itself be
	// evicted in the same call (eviction begins after marking it terminal),
	// so we expect chunks == 6 and one eviction count of 0.
	tr := NewTree(4)
	tr.Insert(h(1, 2, 3, 4, 5, 6))
	// Budget forces eviction; the only terminal is the freshly-inserted one.
	// After the loop runs once, that terminal is evicted, leaving zero chunks.
	if tr.Len() != 0 {
		t.Errorf("Len = %d, want 0 after evicting the only terminal", tr.Len())
	}
	if got := tr.Stats().Evicted; got != 1 {
		t.Errorf("Evicted = %d, want 1", got)
	}
}

func TestEviction_OldestTerminalEvictedFirst(t *testing.T) {
	tr := NewTree(8)         // budget 8 chunks
	tr.Insert(h(1, 2, 3, 4)) // 4 chunks; LRU back: A
	tr.Insert(h(5, 6, 7, 8)) // +4 chunks; total 8; LRU back: A, front: B

	if tr.Len() != 8 || tr.Stats().Evicted != 0 {
		t.Fatalf("after 2 inserts: len=%d evicted=%d", tr.Len(), tr.Stats().Evicted)
	}

	tr.Insert(h(9, 10, 11)) // +3 chunks; over budget; A should be evicted
	if got := tr.LongestMatch(h(1, 2, 3, 4)); got != 0 {
		t.Errorf("oldest prompt should have been fully evicted, match=%d", got)
	}
	if got := tr.LongestMatch(h(5, 6, 7, 8)); got != 4 {
		t.Errorf("middle prompt should still match, got %d", got)
	}
	if got := tr.LongestMatch(h(9, 10, 11)); got != 3 {
		t.Errorf("newest prompt should match, got %d", got)
	}
	if tr.Stats().Evicted == 0 {
		t.Errorf("expected at least one eviction")
	}
}

func TestEviction_TouchPromotesToNewest(t *testing.T) {
	tr := NewTree(8)
	tr.Insert(h(1, 2, 3, 4)) // A
	tr.Insert(h(5, 6, 7, 8)) // B
	// Re-insert A: should promote A to newest.
	tr.Insert(h(1, 2, 3, 4))
	// Now insert C (3 chunks) forcing eviction; B is oldest, should go.
	tr.Insert(h(9, 10, 11))
	if got := tr.LongestMatch(h(1, 2, 3, 4)); got != 4 {
		t.Errorf("A should remain (re-inserted), match=%d", got)
	}
	if got := tr.LongestMatch(h(5, 6, 7, 8)); got != 0 {
		t.Errorf("B should have been evicted (oldest), match=%d", got)
	}
}

func TestEviction_SharedPrefixOnlyTrailFreed(t *testing.T) {
	// Two prompts share the [1,2,3] prefix. When the older is evicted, only
	// its non-shared tail should be freed; the shared prefix must remain.
	tr := NewTree(100)
	tr.Insert(h(1, 2, 3, 4, 5)) // A: 5 chunks
	tr.Insert(h(1, 2, 3, 6, 7)) // B: shares [1,2,3] with A
	// Tree state: split [1,2,3] + [4,5] + [6,7] = 7 chunks total.
	if tr.Len() != 7 {
		t.Fatalf("Len = %d, want 7", tr.Len())
	}
	// Shrink the budget to force evicting A.
	// We can't change maxChunks at runtime, so simulate by making a smaller
	// tree that takes both prompts then receives a third.
	tr2 := NewTree(8)
	tr2.Insert(h(1, 2, 3, 4, 5)) // A
	tr2.Insert(h(1, 2, 3, 6, 7)) // B
	// Now over budget (7 vs 8 ok). Force eviction with another big insert.
	tr2.Insert(h(20, 21, 22)) // C: pushes total above 8
	// Older terminal (A) was evicted; shared [1,2,3] still serves B.
	if got := tr2.LongestMatch(h(1, 2, 3, 6, 7)); got != 5 {
		t.Errorf("B should still match in full, got %d", got)
	}
	if got := tr2.LongestMatch(h(1, 2, 3, 4, 5)); got != 3 {
		t.Errorf("A's tail should be gone, but [1,2,3] shared, got %d (want 3)", got)
	}
}

func TestEviction_StatsTrackEvicted(t *testing.T) {
	tr := NewTree(4)
	tr.Insert(h(1, 2, 3, 4))
	tr.Insert(h(5, 6, 7, 8))
	tr.Insert(h(9, 10, 11, 12))
	st := tr.Stats()
	if st.Evicted < 1 {
		t.Errorf("Stats.Evicted = %d, want >= 1", st.Evicted)
	}
	if st.MaxChunks != 4 {
		t.Errorf("Stats.MaxChunks = %d, want 4", st.MaxChunks)
	}
	if st.Terminals == 0 {
		t.Errorf("Stats.Terminals = 0; expected at least one live terminal")
	}
}

func TestEviction_ConcurrentInserts(t *testing.T) {
	tr := NewTree(64) // tight budget forces lots of eviction
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				tr.Insert(h(base+i, base+i+1, base+i+2, base+i+3))
			}
		}(w * 10000)
	}
	// Concurrent readers: must not panic.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				_ = tr.LongestMatch(h(i, i+1))
			}
		}()
	}
	wg.Wait()

	// Tree must still respect the budget after the dust settles.
	if got := tr.Len(); got > 64 {
		t.Errorf("Len = %d exceeds budget 64", got)
	}
	if tr.Stats().Evicted == 0 {
		t.Errorf("expected evictions under tight budget")
	}
}
