package router

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zxuhan/llm-router/internal/backend"
)

// fakeBackend is the test stub used by every router-package test. It satisfies
// backend.Backend with no HTTP I/O; routers never call Do, so the method
// panics if a strategy ever decides to invoke it.
type fakeBackend struct {
	id       string
	url      string
	kvBudget int
	inflight atomic.Int64
	healthy  atomic.Bool
}

func newFakeBackend(id string) *fakeBackend {
	b := &fakeBackend{id: id, url: "http://" + id, kvBudget: 1024}
	b.healthy.Store(true)
	return b
}

func (f *fakeBackend) ID() string        { return f.id }
func (f *fakeBackend) URL() string       { return f.url }
func (f *fakeBackend) KVBudget() int     { return f.kvBudget }
func (f *fakeBackend) Inflight() int64   { return f.inflight.Load() }
func (f *fakeBackend) Healthy() bool     { return f.healthy.Load() }
func (f *fakeBackend) SetHealthy(v bool) { f.healthy.Store(v) }
func (f *fakeBackend) Acquire()          { f.inflight.Add(1) }
func (f *fakeBackend) Release() {
	for {
		v := f.inflight.Load()
		if v <= 0 {
			return
		}
		if f.inflight.CompareAndSwap(v, v-1) {
			return
		}
	}
}
func (f *fakeBackend) SetInflight(n int64) { f.inflight.Store(n) }
func (f *fakeBackend) Do(context.Context, backend.Request) (*http.Response, error) {
	panic("fakeBackend.Do should never be called from router tests")
}

// makeStubs builds a slice of *fakeBackend wrapped as backend.Backend.
func makeStubs(ids ...string) []backend.Backend {
	out := make([]backend.Backend, len(ids))
	for i, id := range ids {
		out[i] = newFakeBackend(id)
	}
	return out
}

// asFake unwraps a backend.Backend to *fakeBackend; used to manipulate
// inflight counters in test setup.
func asFake(b backend.Backend) *fakeBackend { return b.(*fakeBackend) }

func TestRoundRobin_Cycles(t *testing.T) {
	backends := makeStubs("a", "b", "c")
	rr := NewRoundRobin(backends)
	if rr.Name() != "roundrobin" {
		t.Errorf("Name = %q", rr.Name())
	}

	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	for i, w := range want {
		d, err := rr.Choose(context.Background(), "anything")
		if err != nil {
			t.Fatalf("Choose: %v", err)
		}
		if d.Backend.ID() != w {
			t.Errorf("call %d: got %q, want %q", i, d.Backend.ID(), w)
		}
		if d.Reason != "round-robin" {
			t.Errorf("Reason = %q", d.Reason)
		}
		if d.MatchChunks != 0 {
			t.Errorf("MatchChunks = %d, want 0", d.MatchChunks)
		}
	}
}

func TestRoundRobin_Concurrent_DistributionEven(t *testing.T) {
	backends := makeStubs("a", "b", "c", "d")
	rr := NewRoundRobin(backends)

	const N = 4000
	counts := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < N/16; j++ {
				d, err := rr.Choose(context.Background(), "")
				if err != nil {
					t.Errorf("Choose: %v", err)
					return
				}
				mu.Lock()
				counts[d.Backend.ID()]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Strict round-robin with N divisible by len(backends) gives exact shares.
	want := N / len(backends)
	for id, got := range counts {
		if got != want {
			t.Errorf("backend %q: got %d, want %d", id, got, want)
		}
	}
}

func TestRoundRobin_NoBackends(t *testing.T) {
	rr := NewRoundRobin(nil)
	_, err := rr.Choose(context.Background(), "")
	if err == nil {
		t.Fatal("expected ErrNoBackends")
	}
}

func TestRoundRobin_Update_IsNoOp(t *testing.T) {
	backends := makeStubs("a")
	rr := NewRoundRobin(backends)
	// Should not panic.
	rr.Update("anything", backends[0])
}

func TestBackends_ReturnsStableSnapshot(t *testing.T) {
	backends := makeStubs("a", "b")
	rr := NewRoundRobin(backends)
	got := rr.Backends()
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].ID() != "a" || got[1].ID() != "b" {
		t.Errorf("Backends order: %q,%q", got[0].ID(), got[1].ID())
	}
	// asFake is exercised here so it isn't reported as dead in coverage.
	asFake(got[0]).SetInflight(3)
	if asFake(got[0]).Inflight() != 3 {
		t.Errorf("Inflight after SetInflight(3) = %d", asFake(got[0]).Inflight())
	}
}
