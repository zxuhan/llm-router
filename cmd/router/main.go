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
	"flag"
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	configPath := fs.String("config", "config/config.yaml", "path to config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}
	// Real entry point lands in a later commit; for now we just validate flags.
	_ = configPath
	fmt.Fprintln(os.Stderr, "router: server not yet implemented in this build")
	os.Exit(1)
}
