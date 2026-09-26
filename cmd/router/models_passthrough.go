package main

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/zxuhan/llm-router/internal/backend"
)

// modelPassthrough implements http.Handler for /v1/models.
type modelPassthrough struct {
	backends []backend.Backend
	proxies  map[string]*httputil.ReverseProxy
	logger   *slog.Logger
}

// newModelPassthrough builds one reverse proxy per backend base URL. Workers
// are kept in configured order so "the first worker" is deterministic.
func newModelPassthrough(backends []backend.Backend, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	proxies := make(map[string]*httputil.ReverseProxy, len(backends))
	for _, b := range backends {
		u, err := url.Parse(b.URL())
		if err != nil {
			logger.Warn("models passthrough: skipping unreadable worker url",
				slog.String("backend", b.ID()),
				slog.String("url", b.URL()),
				slog.String("err", err.Error()),
			)
			continue
		}
		proxies[b.ID()] = httputil.NewSingleHostReverseProxy(u)
	}
	return &modelPassthrough{backends: backends, proxies: proxies, logger: logger}
}

// ServeHTTP forwards the request to the first usable worker.
func (h *modelPassthrough) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, b := range h.backends {
		p, ok := h.proxies[b.ID()]
		if !ok || !b.Healthy() {
			continue
		}
		p.ServeHTTP(w, r)
		return
	}
	h.logger.Warn("models passthrough: no healthy backend")
	http.Error(w, `{"error":{"type":"router_unavailable","message":"no healthy backend for /v1/models"}}`, http.StatusServiceUnavailable)
}
