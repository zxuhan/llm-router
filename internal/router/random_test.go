package router

import (
	"context"
	"testing"
)

func TestRandom_DeterministicWithSeededRNG(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	// Cycle through indices 1, 2, 0 in order.
	indices := []int{1, 2, 0, 1, 2, 0}
	step := 0
	r := NewRandom(backends).WithRNG(func(int) int {
		v := indices[step%len(indices)]
		step++
		return v
	})
	if r.Name() != "random" {
		t.Errorf("Name = %q", r.Name())
	}

	want := []string{"b", "c", "a", "b", "c", "a"}
	for i, w := range want {
		d, err := r.Choose(context.Background(), "")
		if err != nil {
			t.Fatalf("Choose: %v", err)
		}
		if d.Backend.ID() != w {
			t.Errorf("call %d: got %q, want %q", i, d.Backend.ID(), w)
		}
		if d.Reason != "random" {
			t.Errorf("Reason = %q", d.Reason)
		}
	}
}

func TestRandom_NoBackends(t *testing.T) {
	r := NewRandom(nil)
	if _, err := r.Choose(context.Background(), ""); err == nil {
		t.Fatal("expected ErrNoBackends")
	}
}

func TestRandom_DefaultRNG_DistributesAcrossAllBackends(t *testing.T) {
	backends := makeStubs("a", "b", "c", "d")
	r := NewRandom(backends)
	seen := map[string]bool{}
	for i := 0; i < 1000 && len(seen) < len(backends); i++ {
		d, err := r.Choose(context.Background(), "")
		if err != nil {
			t.Fatalf("Choose: %v", err)
		}
		seen[d.Backend.ID()] = true
	}
	if len(seen) != len(backends) {
		t.Errorf("only saw %d of %d backends in 1000 calls", len(seen), len(backends))
	}
}

func TestRandom_Update_IsNoOp(t *testing.T) {
	backends := makeStubs("a")
	r := NewRandom(backends)
	r.Update("x", backends[0]) // must not panic
}
