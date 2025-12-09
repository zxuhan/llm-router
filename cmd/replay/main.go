// Command replay fires a JSONL trace at a running router endpoint with
// realistic per-session timing and prints one result line per request.
//
// Usage:
//
//	replay --trace trace.jsonl --endpoint http://127.0.0.1:8080/v1/chat/completions
//
// Sessions run concurrently; turns within a session are strictly ordered.
// The full result table is emitted as JSONL to --out (default stdout).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xzhou/llm-router/internal/trace"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)

	tracePath := fs.String("trace", "", "JSONL trace input (required)")
	endpoint := fs.String("endpoint", "http://127.0.0.1:8080/v1/chat/completions", "router endpoint")
	out := fs.String("out", "-", "results output path (or '-' for stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tracePath == "" {
		return errors.New("--trace is required")
	}

	f, err := os.Open(*tracePath)
	if err != nil {
		return fmt.Errorf("open trace: %w", err)
	}
	tr, err := trace.ReadJSONL(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("parse trace: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	r := &trace.Replayer{Endpoint: *endpoint}
	results, err := r.Replay(ctx, tr)
	if err != nil {
		return err
	}

	var w *os.File
	if *out == "-" {
		w = stdout
	} else {
		f, err := os.Create(*out)
		if err != nil {
			return fmt.Errorf("open output: %w", err)
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, res := range results {
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
	return nil
}
