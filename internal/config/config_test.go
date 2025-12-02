package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault_IsValid(t *testing.T) {
	cfg := Default()
	cfg.Workers = []WorkerConfig{{
		ID: "w0", URL: "http://127.0.0.1:8001", KVBudget: 1024, MaxInflight: 8,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config (with one worker) should validate, got: %v", err)
	}
}

func TestLoadBytes_FullExample(t *testing.T) {
	data := []byte(`
server:
  addr: ":9999"
  read_timeout: 5s
  shutdown_timeout: 2s
router:
  strategy: prefixaware
  chunk_size: 16
  min_match_chunks: 1
  saturation_inflight: 4
workers:
  - id: a
    url: http://127.0.0.1:8001
    kv_budget: 2048
    max_inflight: 4
    timeout: 30s
  - id: b
    url: http://127.0.0.1:8002
    kv_budget: 2048
    max_inflight: 4
logging:
  level: debug
  format: json
metrics:
  enabled: false
  addr: ":9091"
`)
	cfg, err := LoadBytes(data, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("server.addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 5*time.Second {
		t.Errorf("server.read_timeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Router.Strategy != StrategyPrefixAware {
		t.Errorf("router.strategy = %q", cfg.Router.Strategy)
	}
	if len(cfg.Workers) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(cfg.Workers))
	}
	if cfg.Workers[0].Timeout != 30*time.Second {
		t.Errorf("worker[0].timeout = %v", cfg.Workers[0].Timeout)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("logging.format = %q", cfg.Logging.Format)
	}
	if cfg.Metrics.Enabled {
		t.Errorf("metrics.enabled should be false")
	}
}

func TestLoadBytes_EnvOverrides(t *testing.T) {
	base := []byte(`
workers:
  - id: w0
    url: http://127.0.0.1:8001
`)
	env := map[string]string{
		"ROUTER_SERVER_ADDR":         ":7777",
		"ROUTER_STRATEGY":            "leastloaded",
		"ROUTER_CHUNK_SIZE":          "64",
		"ROUTER_MIN_MATCH_CHUNKS":    "3",
		"ROUTER_SATURATION_INFLIGHT": "12",
		"ROUTER_LOG_LEVEL":           "warn",
		"ROUTER_LOG_FORMAT":          "json",
		"ROUTER_METRICS_ADDR":        ":7000",
	}
	cfg, err := LoadBytes(base, lookupFromMap(env))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.Server.Addr != ":7777" {
		t.Errorf("server addr override failed: %q", cfg.Server.Addr)
	}
	if cfg.Router.Strategy != StrategyLeastLoaded {
		t.Errorf("strategy override failed: %q", cfg.Router.Strategy)
	}
	if cfg.Router.ChunkSize != 64 {
		t.Errorf("chunk size override failed: %d", cfg.Router.ChunkSize)
	}
	if cfg.Router.MinMatchChunks != 3 {
		t.Errorf("min match chunks override failed: %d", cfg.Router.MinMatchChunks)
	}
	if cfg.Router.SaturationInflight != 12 {
		t.Errorf("saturation override failed: %d", cfg.Router.SaturationInflight)
	}
	if cfg.Logging.Level != "warn" || cfg.Logging.Format != "json" {
		t.Errorf("logging override failed: %+v", cfg.Logging)
	}
	if cfg.Metrics.Addr != ":7000" {
		t.Errorf("metrics addr override failed: %q", cfg.Metrics.Addr)
	}
}

func TestLoadBytes_BadIntegerEnvVarsAreIgnored(t *testing.T) {
	base := []byte(`
workers:
  - id: w0
    url: http://127.0.0.1:8001
`)
	env := map[string]string{
		"ROUTER_CHUNK_SIZE":          "not-a-number",
		"ROUTER_MIN_MATCH_CHUNKS":    "x",
		"ROUTER_SATURATION_INFLIGHT": "y",
	}
	cfg, err := LoadBytes(base, lookupFromMap(env))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	def := Default()
	if cfg.Router.ChunkSize != def.Router.ChunkSize {
		t.Errorf("chunk size should fall back to default, got %d", cfg.Router.ChunkSize)
	}
	if cfg.Router.MinMatchChunks != def.Router.MinMatchChunks {
		t.Errorf("min match chunks should fall back to default")
	}
}

func TestLoadBytes_UnknownFieldRejected(t *testing.T) {
	data := []byte(`
unexpected: surprise
workers:
  - id: w0
    url: http://127.0.0.1:8001
`)
	_, err := LoadBytes(data, nil)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestLoadBytes_BadYAML(t *testing.T) {
	_, err := LoadBytes([]byte(":\n  not yaml at all: ::"), nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_FromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	body := `
workers:
  - id: w0
    url: http://127.0.0.1:8001
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Workers[0].ID != "w0" {
		t.Errorf("worker id = %q", cfg.Workers[0].ID)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/no/such/file/should/exist.yaml")
	if err == nil {
		t.Fatal("expected error reading missing file")
	}
}

func TestLoad_BadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(path, []byte(":\n  : :"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error %q should mention parse stage", err.Error())
	}
}

func TestLoad_ValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-workers.yaml")
	if err := os.WriteFile(path, []byte("workers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "validate config") {
		t.Errorf("error %q should mention validate stage", err.Error())
	}
}

func TestLoadBytes_ValidationFailure(t *testing.T) {
	_, err := LoadBytes([]byte("workers: []\n"), nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidate_HostlessURL(t *testing.T) {
	cfg := Default()
	cfg.Workers = []WorkerConfig{{ID: "w0", URL: "http:///"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must include a host") {
		t.Fatalf("expected host error, got %v", err)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{
			name: "empty server addr",
			mut:  func(c *Config) { c.Server.Addr = "" },
			want: "server.addr",
		},
		{
			name: "negative shutdown timeout",
			mut:  func(c *Config) { c.Server.ShutdownTimeout = -time.Second },
			want: "shutdown_timeout",
		},
		{
			name: "invalid strategy",
			mut:  func(c *Config) { c.Router.Strategy = "bogus" },
			want: "router.strategy",
		},
		{
			name: "zero chunk size",
			mut:  func(c *Config) { c.Router.ChunkSize = 0 },
			want: "chunk_size",
		},
		{
			name: "negative min match",
			mut:  func(c *Config) { c.Router.MinMatchChunks = -1 },
			want: "min_match_chunks",
		},
		{
			name: "negative saturation",
			mut:  func(c *Config) { c.Router.SaturationInflight = -1 },
			want: "saturation_inflight",
		},
		{
			name: "no workers",
			mut:  func(c *Config) { c.Workers = nil },
			want: "at least one worker",
		},
		{
			name: "duplicate worker ids",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{
					{ID: "w0", URL: "http://x:1"},
					{ID: "w0", URL: "http://y:2"},
				}
			},
			want: "duplicated",
		},
		{
			name: "missing worker id",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{{URL: "http://x:1"}}
			},
			want: "id must be set",
		},
		{
			name: "missing worker url",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{{ID: "w0"}}
			},
			want: "url must be set",
		},
		{
			name: "non-http worker url",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{{ID: "w0", URL: "ftp://x:1"}}
			},
			want: "must be http or https",
		},
		{
			name: "negative kv budget",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{{ID: "w0", URL: "http://x:1", KVBudget: -1}}
			},
			want: "kv_budget",
		},
		{
			name: "negative max inflight",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{{ID: "w0", URL: "http://x:1", MaxInflight: -1}}
			},
			want: "max_inflight",
		},
		{
			name: "negative timeout",
			mut: func(c *Config) {
				c.Workers = []WorkerConfig{{ID: "w0", URL: "http://x:1", Timeout: -time.Second}}
			},
			want: "timeout",
		},
		{
			name: "bad logging level",
			mut:  func(c *Config) { c.Logging.Level = "spam" },
			want: "logging.level",
		},
		{
			name: "bad logging format",
			mut:  func(c *Config) { c.Logging.Format = "yaml" },
			want: "logging.format",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Workers = []WorkerConfig{{ID: "w0", URL: "http://x:1", KVBudget: 1, MaxInflight: 1}}
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAllStrategies_Stable(t *testing.T) {
	got := AllStrategies()
	want := []Strategy{
		StrategyRoundRobin, StrategyRandom, StrategyLeastLoaded, StrategyPrefixAware,
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("AllStrategies()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func lookupFromMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}
