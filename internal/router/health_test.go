package router

import (
	"context"
	"testing"

	"github.com/xzhou/llm-router/internal/backend"
)

// asBackend wraps a *fakeBackend for use as a backend.Backend.
func asBackend(f *fakeBackend) backend.Backend { return f }

// asBackendSlice converts []*fakeBackend to []backend.Backend.
func asBackendSlice(fs []*fakeBackend) []backend.Backend {
	out := make([]backend.Backend, len(fs))
	for i, f := range fs {
		out[i] = f
	}
	return out
}

// All strategies must skip backends that report Healthy() == false. When no
// backends are healthy, Choose returns ErrNoBackends.

func TestStrategies_SkipUnhealthyBackends(t *testing.T) {
	for _, sname := range []string{"roundrobin", "random", "leastloaded", "prefixaware"} {
		t.Run(sname, func(t *testing.T) {
			fakes := []*fakeBackend{newFakeBackend("a"), newFakeBackend("b"), newFakeBackend("c")}
			fakes[1].SetHealthy(false) // "b" is down

			var r Router
			switch sname {
			case "roundrobin":
				r = NewRoundRobin(asBackendSlice(fakes))
			case "random":
				r = NewRandom(asBackendSlice(fakes)).WithRNG(func(int) int { return 0 })
			case "leastloaded":
				r = NewLeastLoaded(asBackendSlice(fakes))
			case "prefixaware":
				r = NewPrefixAware(asBackendSlice(fakes), PrefixAwareOptions{
					Chunker: NewChunker(8), MinMatchChunks: 1,
				})
			}

			seen := map[string]struct{}{}
			for i := 0; i < 30; i++ {
				d, err := r.Choose(context.Background(), "anything shared")
				if err != nil {
					t.Fatalf("Choose: %v", err)
				}
				seen[d.Backend.ID()] = struct{}{}
			}
			if _, ok := seen["b"]; ok {
				t.Errorf("strategy %s sent traffic to unhealthy backend b; saw %v", sname, seen)
			}
			if len(seen) == 0 {
				t.Errorf("strategy %s never produced a decision", sname)
			}
		})
	}
}

func TestStrategies_AllUnhealthyReturnsErrNoBackends(t *testing.T) {
	for _, sname := range []string{"roundrobin", "random", "leastloaded", "prefixaware"} {
		t.Run(sname, func(t *testing.T) {
			fakes := []*fakeBackend{newFakeBackend("a"), newFakeBackend("b")}
			for _, f := range fakes {
				f.SetHealthy(false)
			}
			var r Router
			switch sname {
			case "roundrobin":
				r = NewRoundRobin(asBackendSlice(fakes))
			case "random":
				r = NewRandom(asBackendSlice(fakes))
			case "leastloaded":
				r = NewLeastLoaded(asBackendSlice(fakes))
			case "prefixaware":
				r = NewPrefixAware(asBackendSlice(fakes), PrefixAwareOptions{
					Chunker: NewChunker(8), MinMatchChunks: 1,
				})
			}
			if _, err := r.Choose(context.Background(), "x"); err == nil {
				t.Errorf("strategy %s should return ErrNoBackends when every backend is unhealthy", sname)
			}
		})
	}
}

func TestPrefixAware_RecoveryAfterUnhealthy(t *testing.T) {
	fakes := []*fakeBackend{newFakeBackend("a"), newFakeBackend("b")}
	r := NewPrefixAware(asBackendSlice(fakes), PrefixAwareOptions{
		Chunker: NewChunker(4), MinMatchChunks: 1,
	})

	// Train a's tree, then take a out.
	r.Update("AAAAAAAA", asBackend(fakes[0]))
	fakes[0].SetHealthy(false)
	d, _ := r.Choose(context.Background(), "AAAAAAAA")
	if d.Backend.ID() != "b" {
		t.Errorf("unhealthy a should not be chosen; got %q", d.Backend.ID())
	}

	// a comes back; with MinMatchChunks=1 a still wins because its tree has
	// the prefix and b's does not.
	fakes[0].SetHealthy(true)
	d, _ = r.Choose(context.Background(), "AAAAAAAA")
	if d.Backend.ID() != "a" {
		t.Errorf("recovered a should be chosen; got %q", d.Backend.ID())
	}
}
