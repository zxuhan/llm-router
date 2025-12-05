package router

import (
	"context"
	"sync"
	"testing"
)

func TestLeastLoaded_PicksLowestInflight(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	asFake(backends[0]).SetInflight(5)
	asFake(backends[1]).SetInflight(1)
	asFake(backends[2]).SetInflight(3)

	l := NewLeastLoaded(backends)
	if l.Name() != "leastloaded" {
		t.Errorf("Name = %q", l.Name())
	}

	d, err := l.Choose(context.Background(), "")
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if d.Backend.ID() != "b" {
		t.Errorf("got %q, want b", d.Backend.ID())
	}
	if d.Reason != "least-loaded" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestLeastLoaded_TieBreaksByListOrder(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	// All zero inflight; first backend wins.
	d, err := NewLeastLoaded(backends).Choose(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Backend.ID() != "a" {
		t.Errorf("tie-break: got %q, want a", d.Backend.ID())
	}
}

func TestLeastLoaded_NoBackends(t *testing.T) {
	if _, err := NewLeastLoaded(nil).Choose(context.Background(), ""); err == nil {
		t.Fatal("expected ErrNoBackends")
	}
}

func TestLeastLoaded_AllBackendsLoaded(t *testing.T) {
	backends := makeStubs("a", "b")
	asFake(backends[0]).SetInflight(7)
	asFake(backends[1]).SetInflight(7)
	d, _ := NewLeastLoaded(backends).Choose(context.Background(), "")
	if d.Backend.ID() != "a" {
		t.Errorf("equal load tie-break: got %q, want a", d.Backend.ID())
	}
}

func TestLeastLoaded_ReactsToChangingLoad(t *testing.T) {
	backends := makeStubs("a", "b")
	l := NewLeastLoaded(backends)

	asFake(backends[0]).SetInflight(0)
	asFake(backends[1]).SetInflight(2)
	if d, _ := l.Choose(context.Background(), ""); d.Backend.ID() != "a" {
		t.Errorf("first: got %q", d.Backend.ID())
	}

	asFake(backends[0]).SetInflight(5)
	asFake(backends[1]).SetInflight(2)
	if d, _ := l.Choose(context.Background(), ""); d.Backend.ID() != "b" {
		t.Errorf("after load shift: got %q", d.Backend.ID())
	}
}

func TestLeastLoaded_Concurrent_NoRace(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	l := NewLeastLoaded(backends)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				asFake(backends[idx%3]).Acquire()
				_, _ = l.Choose(context.Background(), "")
				asFake(backends[idx%3]).Release()
			}
		}(i)
	}
	wg.Wait()
}

func TestLeastLoaded_Update_IsNoOp(t *testing.T) {
	backends := makeStubs("a")
	l := NewLeastLoaded(backends)
	l.Update("x", backends[0])
}
