// Command router runs the prefix-cache aware LLM router.
//
// It accepts OpenAI-compatible /v1/chat/completions requests and forwards them
// to one of N configured llama.cpp (or compatible) backend workers, selecting
// the worker by a configurable strategy. The default strategy is "prefix-aware",
// which biases toward the worker whose KV cache is most likely to already hold
// the request's token prefix.
//
// Usage:
//
//	router --config config/config.yaml
//
// See README.md for the full thesis, supported strategies, and limitations.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zxuhan/llm-router/internal/backend"
	"github.com/zxuhan/llm-router/internal/config"
	"github.com/zxuhan/llm-router/internal/logging"
	"github.com/zxuhan/llm-router/internal/metrics"
	"github.com/zxuhan/llm-router/internal/proxy"
	"github.com/zxuhan/llm-router/internal/router"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "router:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	configPath := fs.String("config", "config/config.yaml", "path to config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := logging.New(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return err
	}
	logger.Info("starting router",
		slog.String("version", version),
		slog.String("config", *configPath),
		slog.String("strategy", string(cfg.Router.Strategy)),
		slog.Int("workers", len(cfg.Workers)),
	)

	backends, err := buildBackends(cfg.Workers)
	if err != nil {
		return err
	}

	rt, err := buildRouter(cfg.Router, backends)
	if err != nil {
		return err
	}

	reg := metrics.New()
	reg.RegisterRuntime()
	if pa, ok := rt.(*router.PrefixAware); ok {
		reg.RegisterPrefixTrees(pa)
	}

	recorder := combineRecorders(reg.Recorder(), logging.AccessLogRecorder(logger))
	handler, err := proxy.New(proxy.Options{
		Router:   rt,
		Recorder: recorder,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mainSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	var metricsSrv *http.Server
	if cfg.Metrics.Enabled {
		mm := http.NewServeMux()
		mm.Handle("/metrics", reg.Handler())
		mm.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		metricsSrv = &http.Server{Addr: cfg.Metrics.Addr, Handler: mm}
	}

	return serveUntilSignal(logger, cfg.Server.ShutdownTimeout, mainSrv, metricsSrv)
}

// buildBackends materialises a Backend per worker configuration.
func buildBackends(workers []config.WorkerConfig) ([]backend.Backend, error) {
	out := make([]backend.Backend, 0, len(workers))
	for _, w := range workers {
		b, err := backend.NewLlamaCpp(backend.LlamaCppOptions{
			ID:                      w.ID,
			URL:                     w.URL,
			KVBudget:                w.KVBudget,
			Timeout:                 w.Timeout,
			CircuitBreakerThreshold: w.BreakerThreshold,
			CircuitBreakerCooldown:  w.BreakerCooldown,
		})
		if err != nil {
			return nil, fmt.Errorf("worker %s: %w", w.ID, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// buildRouter constructs the strategy named in cfg from the given backends.
func buildRouter(cfg config.RouterConfig, backends []backend.Backend) (router.Router, error) {
	switch cfg.Strategy {
	case config.StrategyRoundRobin:
		return router.NewRoundRobin(backends), nil
	case config.StrategyRandom:
		return router.NewRandom(backends), nil
	case config.StrategyLeastLoaded:
		return router.NewLeastLoaded(backends), nil
	case config.StrategyPrefixAware:
		return router.NewPrefixAware(backends, router.PrefixAwareOptions{
			Chunker:            router.NewChunker(cfg.ChunkSize),
			MinMatchChunks:     cfg.MinMatchChunks,
			SaturationInflight: cfg.SaturationInflight,
		}), nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", cfg.Strategy)
	}
}

// combineRecorders returns a Recorder that fans out to each input recorder.
func combineRecorders(recs ...proxy.Recorder) proxy.Recorder {
	return func(s proxy.RequestStats) {
		for _, r := range recs {
			r(s)
		}
	}
}

// serveUntilSignal starts the supplied servers and blocks until SIGINT or
// SIGTERM is received, at which point it gracefully shuts each one down.
func serveUntilSignal(logger *slog.Logger, shutdownTimeout time.Duration, servers ...*http.Server) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(servers))

	for _, s := range servers {
		if s == nil {
			continue
		}
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			logger.Info("listening", slog.String("addr", s.Addr))
			err := s.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s)
	}

	<-ctx.Done()
	logger.Info("shutdown signal received; draining")

	shCtx, shCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shCancel()
	for _, s := range servers {
		if s == nil {
			continue
		}
		if err := s.Shutdown(shCtx); err != nil {
			logger.Warn("server shutdown error", slog.String("err", err.Error()))
		}
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
