// Package config loads and validates the router's runtime configuration.
//
// Configuration is read from a YAML file on disk; a small whitelist of fields
// can be overridden via environment variables (prefix ROUTER_) for ergonomic
// twelve-factor-style operation. Loading is intentionally strict: unknown YAML
// fields cause an error, and validation runs before the loader returns, so any
// process that successfully calls Load is holding a struct it can rely on.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Strategy enumerates the supported routing strategies.
type Strategy string

const (
	StrategyRoundRobin  Strategy = "roundrobin"
	StrategyRandom      Strategy = "random"
	StrategyLeastLoaded Strategy = "leastloaded"
	StrategyPrefixAware Strategy = "prefixaware"
)

// AllStrategies enumerates the supported routing strategies in their canonical
// order. Used for CLI help text, generated docs, and validation messages.
func AllStrategies() []Strategy {
	return []Strategy{
		StrategyRoundRobin,
		StrategyRandom,
		StrategyLeastLoaded,
		StrategyPrefixAware,
	}
}

// Config is the top-level configuration object.
type Config struct {
	Server  ServerConfig   `yaml:"server"`
	Router  RouterConfig   `yaml:"router"`
	Workers []WorkerConfig `yaml:"workers"`
	Logging LoggingConfig  `yaml:"logging"`
	Metrics MetricsConfig  `yaml:"metrics"`
}

// ServerConfig configures the front-facing HTTP server.
type ServerConfig struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// RouterConfig configures the routing strategy and its tunables.
type RouterConfig struct {
	Strategy           Strategy `yaml:"strategy"`
	ChunkSize          int      `yaml:"chunk_size"`          // chars per hashed chunk
	MinMatchChunks     int      `yaml:"min_match_chunks"`    // threshold for prefix-aware
	SaturationInflight int      `yaml:"saturation_inflight"` // safety-valve threshold
}

// WorkerConfig describes a single backend worker.
type WorkerConfig struct {
	ID          string        `yaml:"id"`
	URL         string        `yaml:"url"`
	KVBudget    int           `yaml:"kv_budget"`    // approx tokens (chunks) per worker
	MaxInflight int           `yaml:"max_inflight"` // 0 disables the cap
	Timeout     time.Duration `yaml:"timeout"`      // upstream request timeout
	// BreakerThreshold is the number of consecutive 5xx/transport failures
	// that trips this backend out of rotation. Zero falls back to the
	// circuit breaker's internal default (5).
	BreakerThreshold int `yaml:"breaker_threshold"`
	// BreakerCooldown is how long the breaker stays open before
	// auto-resetting. Zero falls back to the breaker's default (30s).
	BreakerCooldown time.Duration `yaml:"breaker_cooldown"`
}

// LoggingConfig configures the structured logger.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

// MetricsConfig configures the Prometheus metrics endpoint.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// Default returns a Config populated with reasonable production defaults. Tests
// and the CLI both rely on this; changing a default is a behavioural change and
// should be reviewed.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:            ":8080",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    0,
			ShutdownTimeout: 10 * time.Second,
		},
		Router: RouterConfig{
			Strategy:           StrategyPrefixAware,
			ChunkSize:          32,
			MinMatchChunks:     2,
			SaturationInflight: 8,
		},
		Logging: LoggingConfig{Level: "info", Format: "text"},
		Metrics: MetricsConfig{Enabled: true, Addr: ":9090"},
	}
}

// Load reads a YAML file from path, applies environment overrides, validates,
// and returns the resulting Config. Missing optional fields fall back to
// Default() values; the file does not need to be exhaustive.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := unmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	applyEnvOverrides(&cfg, os.LookupEnv)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

// LoadBytes is the test-friendly counterpart of Load: it accepts raw YAML bytes
// and an env lookup function.
func LoadBytes(data []byte, lookup func(string) (string, bool)) (Config, error) {
	cfg := Default()
	if err := unmarshalStrict(data, &cfg); err != nil {
		return Config{}, err
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	applyEnvOverrides(&cfg, lookup)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func unmarshalStrict(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	return nil
}

// applyEnvOverrides mutates cfg in place from a small whitelist of env vars.
// We deliberately keep the surface tiny; richer overrides belong in the YAML.
func applyEnvOverrides(cfg *Config, lookup func(string) (string, bool)) {
	if v, ok := lookup("ROUTER_SERVER_ADDR"); ok && v != "" {
		cfg.Server.Addr = v
	}
	if v, ok := lookup("ROUTER_STRATEGY"); ok && v != "" {
		cfg.Router.Strategy = Strategy(v)
	}
	if v, ok := lookup("ROUTER_CHUNK_SIZE"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Router.ChunkSize = n
		}
	}
	if v, ok := lookup("ROUTER_MIN_MATCH_CHUNKS"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Router.MinMatchChunks = n
		}
	}
	if v, ok := lookup("ROUTER_SATURATION_INFLIGHT"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Router.SaturationInflight = n
		}
	}
	if v, ok := lookup("ROUTER_LOG_LEVEL"); ok && v != "" {
		cfg.Logging.Level = v
	}
	if v, ok := lookup("ROUTER_LOG_FORMAT"); ok && v != "" {
		cfg.Logging.Format = v
	}
	if v, ok := lookup("ROUTER_METRICS_ADDR"); ok && v != "" {
		cfg.Metrics.Addr = v
	}
}

// Validate returns nil if the config is internally consistent and the router
// would be safe to start with these values. It accumulates errors into a single
// joined error so the operator sees the full set in one shot.
func (c Config) Validate() error {
	var errs []error
	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr must be set"))
	}
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must be non-negative"))
	}

	if !isValidStrategy(c.Router.Strategy) {
		errs = append(errs, fmt.Errorf("router.strategy %q is not one of %v",
			c.Router.Strategy, AllStrategies()))
	}
	if c.Router.ChunkSize <= 0 {
		errs = append(errs, errors.New("router.chunk_size must be > 0"))
	}
	if c.Router.MinMatchChunks < 0 {
		errs = append(errs, errors.New("router.min_match_chunks must be >= 0"))
	}
	if c.Router.SaturationInflight < 0 {
		errs = append(errs, errors.New("router.saturation_inflight must be >= 0"))
	}

	if len(c.Workers) == 0 {
		errs = append(errs, errors.New("at least one worker must be configured"))
	}
	seen := make(map[string]struct{}, len(c.Workers))
	for i, w := range c.Workers {
		ctx := fmt.Sprintf("workers[%d]", i)
		if w.ID == "" {
			errs = append(errs, fmt.Errorf("%s.id must be set", ctx))
		} else if _, dup := seen[w.ID]; dup {
			errs = append(errs, fmt.Errorf("%s.id %q is duplicated", ctx, w.ID))
		} else {
			seen[w.ID] = struct{}{}
		}
		if w.URL == "" {
			errs = append(errs, fmt.Errorf("%s.url must be set", ctx))
		} else {
			u, err := url.Parse(w.URL)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s.url %q: %w", ctx, w.URL, err))
			case u.Scheme != "http" && u.Scheme != "https":
				errs = append(errs, fmt.Errorf("%s.url %q must be http or https", ctx, w.URL))
			case u.Host == "":
				errs = append(errs, fmt.Errorf("%s.url %q must include a host", ctx, w.URL))
			}
		}
		if w.KVBudget < 0 {
			errs = append(errs, fmt.Errorf("%s.kv_budget must be >= 0", ctx))
		}
		if w.MaxInflight < 0 {
			errs = append(errs, fmt.Errorf("%s.max_inflight must be >= 0", ctx))
		}
		if w.Timeout < 0 {
			errs = append(errs, fmt.Errorf("%s.timeout must be >= 0", ctx))
		}
		if w.BreakerThreshold < 0 {
			errs = append(errs, fmt.Errorf("%s.breaker_threshold must be >= 0", ctx))
		}
		if w.BreakerCooldown < 0 {
			errs = append(errs, fmt.Errorf("%s.breaker_cooldown must be >= 0", ctx))
		}
	}

	switch strings.ToLower(c.Logging.Level) {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("logging.level %q is not debug|info|warn|error", c.Logging.Level))
	}
	switch strings.ToLower(c.Logging.Format) {
	case "", "text", "json":
	default:
		errs = append(errs, fmt.Errorf("logging.format %q is not text|json", c.Logging.Format))
	}

	return errors.Join(errs...)
}

func isValidStrategy(s Strategy) bool {
	for _, v := range AllStrategies() {
		if v == s {
			return true
		}
	}
	return false
}
