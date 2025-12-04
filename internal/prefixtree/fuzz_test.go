package prefixtree

import (
	"encoding/binary"
	"testing"
)

// referenceTree is a deliberately naive O(N*L) implementation used as an
// oracle for cross-checking the radix tree's LongestMatch.
type referenceTree struct {
	seqs [][]uint64
}

func (r *referenceTree) Insert(seq []uint64) {
	cp := append([]uint64(nil), seq...)
	r.seqs = append(r.seqs, cp)
}

func (r *referenceTree) LongestMatch(query []uint64) int {
	best := 0
	for _, s := range r.seqs {
		if c := commonPrefix(s, query); c > best {
			best = c
		}
	}
	return best
}

// bytesToSeq turns raw fuzzer bytes into a uint64 sequence in a way that does
// not waste entropy: each byte contributes one chunk-hash. This stresses the
// branchy paths (frequent splits) more than packing 8 bytes per uint64 would.
func bytesToSeq(b []byte) []uint64 {
	out := make([]uint64, len(b))
	for i, x := range b {
		out[i] = uint64(x)
	}
	return out
}

// bytesToSeqWide packs bytes 4-at-a-time so we also exercise edges with larger
// alphabets (less branching, deeper edges).
func bytesToSeqWide(b []byte) []uint64 {
	if len(b) == 0 {
		return nil
	}
	pad := (4 - len(b)%4) % 4
	padded := make([]byte, len(b)+pad)
	copy(padded, b)
	out := make([]uint64, len(padded)/4)
	for i := range out {
		out[i] = uint64(binary.LittleEndian.Uint32(padded[i*4:]))
	}
	return out
}

func FuzzTree_LongestMatchAgainstReference(f *testing.F) {
	f.Add([]byte("abc"), []byte("abd"))
	f.Add([]byte(""), []byte(""))
	f.Add([]byte("a"), []byte("a"))
	f.Add([]byte("aaaa"), []byte("aaab"))
	f.Add([]byte{0, 1, 2, 3, 4, 5}, []byte{0, 1, 2, 3, 9, 9})

	f.Fuzz(func(t *testing.T, a, b []byte) {
		// Bound input size to keep the fuzz fast even when the corpus grows.
		if len(a) > 256 {
			a = a[:256]
		}
		if len(b) > 256 {
			b = b[:256]
		}
		seqA := bytesToSeq(a)
		seqB := bytesToSeq(b)

		tr := NewTree(0)
		ref := &referenceTree{}

		tr.Insert(seqA)
		ref.Insert(seqA)
		tr.Insert(seqB)
		ref.Insert(seqB)

		// Query a range of slices including past-end and empties.
		queries := []([]uint64){seqA, seqB, nil, seqA[:len(seqA)/2], seqB[:len(seqB)/2]}
		for _, q := range queries {
			gotT := tr.LongestMatch(q)
			gotR := ref.LongestMatch(q)
			if gotT != gotR {
				t.Fatalf("LongestMatch(%v) = %d (tree) vs %d (ref); inserts=[%v, %v]",
					q, gotT, gotR, seqA, seqB)
			}
		}
	})
}

func FuzzTree_InsertOrderInvariance(f *testing.F) {
	f.Add([]byte("abcd"), []byte("abef"), []byte("xyzw"))

	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		if len(a) > 128 {
			a = a[:128]
		}
		if len(b) > 128 {
			b = b[:128]
		}
		if len(c) > 128 {
			c = c[:128]
		}
		seqs := [][]uint64{bytesToSeqWide(a), bytesToSeqWide(b), bytesToSeqWide(c)}

		// Insert in two orders; matches must agree on every original prompt.
		t1 := NewTree(0)
		t2 := NewTree(0)
		for _, s := range seqs {
			t1.Insert(s)
		}
		for i := len(seqs) - 1; i >= 0; i-- {
			t2.Insert(seqs[i])
		}
		for _, s := range seqs {
			a := t1.LongestMatch(s)
			b := t2.LongestMatch(s)
			if a != b {
				t.Fatalf("order dependence: %d vs %d for %v", a, b, s)
			}
			if a != len(s) {
				t.Fatalf("inserted prompt did not fully match: got %d want %d", a, len(s))
			}
		}
	})
}

func FuzzTree_EvictionKeepsBudget(f *testing.F) {
	f.Add([]byte("seed-data"), uint8(8))

	f.Fuzz(func(t *testing.T, raw []byte, budget uint8) {
		// Generate up to 10 small prompts, all derived from raw.
		maxBudget := 64
		b := int(budget)
		if b == 0 {
			b = 1
		}
		if b > maxBudget {
			b = maxBudget
		}
		tr := NewTree(b)
		// Make up to 10 short, related sequences out of raw to trigger sharing.
		for i := 0; i < 10 && (i+1)*4 <= len(raw); i++ {
			seq := bytesToSeq(raw[i*4 : (i+1)*4])
			tr.Insert(seq)
		}
		if got := tr.Len(); got > b {
			t.Fatalf("Len = %d exceeds budget %d", got, b)
		}
	})
}
