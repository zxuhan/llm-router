package prefixtree

import (
	"sync"
	"testing"
)

// h returns a sequence of uint64 chunk hashes from a varargs list of ints,
// keeping tests readable.
func h(xs ...int) []uint64 {
	out := make([]uint64, len(xs))
	for i, x := range xs {
		out[i] = uint64(x)
	}
	return out
}

func TestTree_EmptySequence(t *testing.T) {
	tr := NewTree()
	tr.Insert(h())
	if got := tr.LongestMatch(h()); got != 0 {
		t.Errorf("LongestMatch on empty = %d, want 0", got)
	}
	if tr.Len() != 0 {
		t.Errorf("Len = %d, want 0", tr.Len())
	}
}

func TestTree_SingleInsertAndMatch(t *testing.T) {
	tr := NewTree()
	tr.Insert(h(1, 2, 3, 4))

	tests := []struct {
		name  string
		query []uint64
		want  int
	}{
		{"exact", h(1, 2, 3, 4), 4},
		{"prefix", h(1, 2), 2},
		{"longer than insert", h(1, 2, 3, 4, 5, 6), 4},
		{"diverges at position 2", h(1, 2, 9, 9), 2},
		{"no match at root", h(9, 9), 0},
		{"empty", h(), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tr.LongestMatch(tc.query)
			if got != tc.want {
				t.Errorf("LongestMatch(%v) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
	if tr.Len() != 4 {
		t.Errorf("Len = %d, want 4", tr.Len())
	}
}

func TestTree_SharedPrefixThenDivergeSplitsEdge(t *testing.T) {
	tr := NewTree()
	tr.Insert(h(1, 2, 3, 4))
	tr.Insert(h(1, 2, 5, 6))

	if got := tr.LongestMatch(h(1, 2, 3, 4)); got != 4 {
		t.Errorf("first prompt match = %d", got)
	}
	if got := tr.LongestMatch(h(1, 2, 5, 6)); got != 4 {
		t.Errorf("second prompt match = %d", got)
	}
	if got := tr.LongestMatch(h(1, 2, 7)); got != 2 {
		t.Errorf("diverging at 2 = %d", got)
	}
	// Tree should hold: [1,2] (split) + [3,4] + [5,6] = 6 chunks.
	if tr.Len() != 6 {
		t.Errorf("Len = %d, want 6", tr.Len())
	}
}

func TestTree_InsertWhereSeqEndsAtSplit(t *testing.T) {
	// Existing edge is [1,2,3,4]; new prompt is exactly [1,2].
	tr := NewTree()
	tr.Insert(h(1, 2, 3, 4))
	tr.Insert(h(1, 2))

	if got := tr.LongestMatch(h(1, 2)); got != 2 {
		t.Errorf("short prefix match = %d, want 2", got)
	}
	if got := tr.LongestMatch(h(1, 2, 3, 4)); got != 4 {
		t.Errorf("long prefix match = %d, want 4", got)
	}
	if tr.Len() != 4 {
		t.Errorf("Len = %d, want 4", tr.Len())
	}
}

func TestTree_InsertExtendsExistingTerminal(t *testing.T) {
	tr := NewTree()
	tr.Insert(h(1, 2))
	tr.Insert(h(1, 2, 3, 4))

	if got := tr.LongestMatch(h(1, 2)); got != 2 {
		t.Errorf("short = %d, want 2", got)
	}
	if got := tr.LongestMatch(h(1, 2, 3, 4)); got != 4 {
		t.Errorf("extended = %d, want 4", got)
	}
	if tr.Len() != 4 {
		t.Errorf("Len = %d, want 4", tr.Len())
	}
}

func TestTree_DuplicateInsertIsIdempotent(t *testing.T) {
	tr := NewTree()
	tr.Insert(h(1, 2, 3))
	tr.Insert(h(1, 2, 3))
	if tr.Len() != 3 {
		t.Errorf("Len = %d, want 3", tr.Len())
	}
	if got := tr.LongestMatch(h(1, 2, 3)); got != 3 {
		t.Errorf("match = %d, want 3", got)
	}
}

func TestTree_BranchingTreeStatsCount(t *testing.T) {
	tr := NewTree()
	tr.Insert(h(1, 2))
	tr.Insert(h(1, 3))
	tr.Insert(h(4, 5, 6))

	st := tr.Stats()
	if st.Inserts != 3 {
		t.Errorf("Stats.Inserts = %d", st.Inserts)
	}
	if got := tr.LongestMatch(h(1, 2)); got != 2 {
		t.Errorf("match 1,2 = %d", got)
	}
	if got := tr.LongestMatch(h(4, 5)); got != 2 {
		t.Errorf("match 4,5 = %d", got)
	}
	st = tr.Stats()
	if st.Queries < 2 {
		t.Errorf("Stats.Queries = %d", st.Queries)
	}
}

func TestTree_Concurrent_ReadersAndWriters(t *testing.T) {
	tr := NewTree()
	// Pre-populate.
	for i := 0; i < 256; i++ {
		seq := h(i, i+1, i+2)
		tr.Insert(seq)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = tr.LongestMatch(h(seed, seed+1, seed+2))
				_ = tr.Len()
			}
		}(r % 256)
	}
	// Writers.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.Insert(h(base+i, base+i+1, base+i+2))
			}
		}(w * 1000)
	}
	// Let everything churn briefly.
	for i := 0; i < 1000; i++ {
		_ = tr.LongestMatch(h(i, i+1))
	}
	close(stop)
	wg.Wait()
}

func TestCommonPrefix(t *testing.T) {
	cases := []struct {
		a, b []uint64
		want int
	}{
		{nil, nil, 0},
		{h(1), nil, 0},
		{h(1), h(1), 1},
		{h(1, 2, 3), h(1, 2, 3, 4), 3},
		{h(1, 2, 3, 4), h(1, 2, 3), 3},
		{h(1, 2, 3), h(1, 9, 3), 1},
		{h(9, 2, 3), h(1, 2, 3), 0},
	}
	for _, c := range cases {
		got := commonPrefix(c.a, c.b)
		if got != c.want {
			t.Errorf("commonPrefix(%v,%v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
