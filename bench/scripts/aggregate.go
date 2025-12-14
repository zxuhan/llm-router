//go:build ignore

// aggregate reads N per-strategy summary JSONs (each containing a single
// summary) and writes one Markdown document on stdout. Used by
// bench/scripts/real-llm.sh; gated by `//go:build ignore` so it does not
// participate in `go build ./...`.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/xzhou/llm-router/internal/trace"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: aggregate <file.json> [<file.json> ...]")
		os.Exit(2)
	}
	var summaries []trace.Summary
	for _, p := range os.Args[1:] {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", p, err)
			os.Exit(1)
		}
		var doc struct {
			Summaries []trace.Summary `json:"summaries"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			fmt.Fprintf(os.Stderr, "parse %s: %v\n", p, err)
			os.Exit(1)
		}
		summaries = append(summaries, doc.Summaries...)
	}
	if err := trace.WriteMarkdown(summaries, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "write markdown:", err)
		os.Exit(1)
	}
}
