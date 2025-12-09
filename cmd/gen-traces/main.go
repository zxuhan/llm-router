// Command gen-traces emits a synthetic agent trace as JSONL.
//
// The trace is fully deterministic given the same flags: equal flags produce
// byte-equal output, which keeps benchmark runs reproducible.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/xzhou/llm-router/internal/trace"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gen-traces:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("gen-traces", flag.ContinueOnError)
	fs.SetOutput(stderr)

	out := fs.String("out", "-", "output file path (or '-' for stdout)")
	seed := fs.Int64("seed", 42, "RNG seed")
	sessions := fs.Int("sessions", 8, "number of sessions")
	turns := fs.Int("turns", 6, "turns per session")
	sysLen := fs.Int("system-len", 512, "system-prompt length in chars")
	userLen := fs.Int("user-len", 80, "user-message length in chars")
	codeLen := fs.Int("code-context-len", 2048, "code-context length in chars")
	codeShare := fs.Float64("code-share", 0.5, "fraction of sessions using the code-edit pattern")
	toolProb := fs.Float64("tool-prob", 0.3, "probability a turn includes a tool-call expansion")
	jitter := fs.Int("start-jitter-ms", 800, "max random ms between session starts")
	gap := fs.Int("turn-gap-ms", 150, "ms between turns within a session")
	summary := fs.Bool("summary", false, "print a stderr summary after writing")

	if err := fs.Parse(args); err != nil {
		return err
	}

	tr := trace.Generate(trace.Options{
		Seed:                 *seed,
		Sessions:             *sessions,
		TurnsPerSession:      *turns,
		SharedSystemLen:      *sysLen,
		UserTurnLen:          *userLen,
		CodeContextLen:       *codeLen,
		CodeSessionShare:     *codeShare,
		ToolLoopProb:         *toolProb,
		SessionStartJitterMs: *jitter,
		TurnGapMs:            *gap,
	})

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
	if err := trace.WriteJSONL(tr, w); err != nil {
		return fmt.Errorf("write jsonl: %w", err)
	}

	if *summary {
		st := trace.Shape(tr)
		fmt.Fprintf(stderr,
			"trace: %d requests across %d sessions; max delay %v; mean prompt len %d chars\n",
			st.Requests, st.Sessions, st.MaxDelay, st.MeanContentChars)
		fmt.Fprintf(stderr, "patterns: ")
		for k, v := range st.PatternHistogram {
			fmt.Fprintf(stderr, "%s=%d ", k, v)
		}
		fmt.Fprintln(stderr)
	}
	return nil
}
