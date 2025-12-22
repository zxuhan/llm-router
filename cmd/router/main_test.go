package main

import (
	"testing"

	"github.com/zxuhan/llm-router/internal/backend"
	"github.com/zxuhan/llm-router/internal/config"
	"github.com/zxuhan/llm-router/internal/proxy"
)

func TestBuildBackends_HappyPath(t *testing.T) {
	bs, err := buildBackends([]config.WorkerConfig{
		{ID: "a", URL: "http://127.0.0.1:8001", KVBudget: 1024},
		{ID: "b", URL: "http://127.0.0.1:8002", KVBudget: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].ID() != "a" || bs[1].ID() != "b" {
		t.Errorf("unexpected backends: %+v", bs)
	}
}

func TestBuildBackends_BadURL(t *testing.T) {
	if _, err := buildBackends([]config.WorkerConfig{{ID: "x", URL: "ftp://wrong"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildRouter_AllStrategies(t *testing.T) {
	bs, _ := buildBackends([]config.WorkerConfig{{ID: "a", URL: "http://127.0.0.1:8001"}})
	for _, s := range config.AllStrategies() {
		_, err := buildRouter(config.RouterConfig{Strategy: s, ChunkSize: 32}, bs)
		if err != nil {
			t.Errorf("strategy %s: %v", s, err)
		}
	}
}

func TestBuildRouter_UnknownStrategy(t *testing.T) {
	bs, _ := buildBackends([]config.WorkerConfig{{ID: "a", URL: "http://127.0.0.1:8001"}})
	if _, err := buildRouter(config.RouterConfig{Strategy: "unknown", ChunkSize: 32}, bs); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestCombineRecorders_FansOut(t *testing.T) {
	calls := 0
	rec := combineRecorders(
		func(proxy.RequestStats) { calls++ },
		func(proxy.RequestStats) { calls++ },
	)
	rec(proxy.RequestStats{})
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRun_VersionFlag(t *testing.T) {
	if err := run([]string{"-version"}); err != nil {
		t.Errorf("run -version: %v", err)
	}
}

func TestRun_UnknownFlag(t *testing.T) {
	if err := run([]string{"--no-such"}); err == nil {
		t.Fatal("expected flag parse error")
	}
}

// We deliberately skip a happy-path TestRun: it would require booting a
// fake server, listening on a real port, and managing a SIGTERM cleanup
// path - this duplicates what's already covered by internal/integration.
func TestRun_BadConfigPath(t *testing.T) {
	if err := run([]string{"--config", "/no/such/file"}); err == nil {
		t.Fatal("expected error opening missing config")
	}
}

// silence unused-import warning when integration tests trim imports.
var _ = backend.LlamaCppOptions{}
