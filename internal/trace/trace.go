// Package trace generates and replays synthetic agent traces against the
// router. A trace is a stream of OpenAI-compatible chat completion requests
// with realistic timing and shared-prefix structure.
//
// The generator (this file) is deterministic given a seed; the replayer lives
// in replay.go and uses a real HTTP client.
package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Message mirrors the subset of the OpenAI chat schema we need.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// Body is the request body sent to the router. Keeping it explicit (rather
// than a generic map) keeps the generator's output schema stable.
type Body struct {
	Model    string    `json:"model,omitempty"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// Request is one entry in the trace. DelayMs is relative to the trace start;
// the replayer fires the request at that offset.
type Request struct {
	SessionID string `json:"session_id"`
	DelayMs   int64  `json:"delay_ms"`
	Pattern   string `json:"pattern"`
	Body      Body   `json:"body"`
}

// Trace is a list of Requests already sorted by DelayMs ascending.
type Trace struct {
	Requests []Request `json:"requests"`
}

// Options configure the synthetic generator.
type Options struct {
	Seed                 int64   // 0 uses a fixed default for reproducibility
	Sessions             int     // distinct conversation sessions
	TurnsPerSession      int     // turns per session (assistant + user pair = 1 turn)
	SharedSystemLen      int     // characters of shared system prompt
	UserTurnLen          int     // characters of each user message
	ToolLoopProb         float64 // probability that a turn is a tool-call expansion
	CodeContextLen       int     // size in chars of the code-edit context block
	CodeSessionShare     float64 // fraction of sessions that use the code-edit pattern
	SessionStartJitterMs int     // random offset between session start times
	TurnGapMs            int     // mean gap between turns in a session
}

// Default returns a sensible Options for a 3-4 second trace.
func Default() Options {
	return Options{
		Seed:                 42,
		Sessions:             8,
		TurnsPerSession:      6,
		SharedSystemLen:      512,
		UserTurnLen:          80,
		ToolLoopProb:         0.3,
		CodeContextLen:       2048,
		CodeSessionShare:     0.5,
		SessionStartJitterMs: 800,
		TurnGapMs:            150,
	}
}

// Generate produces a deterministic trace. Equal Options produce byte-equal
// traces - this is critical for reproducible benchmarks.
func Generate(opts Options) Trace {
	if opts.Sessions <= 0 {
		opts.Sessions = 1
	}
	if opts.TurnsPerSession <= 0 {
		opts.TurnsPerSession = 1
	}
	if opts.UserTurnLen <= 0 {
		opts.UserTurnLen = 1
	}
	rng := newRng(opts.Seed)

	sysPrompt := makeFixedString("system: be helpful and concise. ", opts.SharedSystemLen, 's')
	codeCtx := makeFixedString("// code context\n", opts.CodeContextLen, 'c')

	var reqs []Request
	for i := 0; i < opts.Sessions; i++ {
		sid := fmt.Sprintf("s-%03d", i)
		useCode := rng.Float64() < opts.CodeSessionShare
		startMs := int64(rng.IntN(opts.SessionStartJitterMs + 1))
		messages := []Message{{Role: "system", Content: sysPrompt}}
		if useCode {
			// A second system message with a large code context. Identical
			// across all turns and shared across "code" sessions.
			messages = append(messages, Message{Role: "system", Content: codeCtx})
		}

		// Build the multi-turn history.
		for turn := 0; turn < opts.TurnsPerSession; turn++ {
			user := fmt.Sprintf("turn %d: %s", turn,
				makeRandomString(rng, opts.UserTurnLen, 'u'+byte(i%6)))
			messages = append(messages, Message{Role: "user", Content: user})

			// Optionally branch into a tool-loop within this turn.
			pattern := "multi-turn"
			if rng.Float64() < opts.ToolLoopProb {
				pattern = "tool-loop"
				messages = append(messages,
					Message{Role: "assistant", Content: fmt.Sprintf("calling tool[%d]", turn)},
					Message{Role: "tool", Name: "search", Content: makeRandomString(rng, 64, 't')},
				)
			}

			// Snapshot the messages-so-far as the turn's request body.
			body := Body{
				Model:    "fake-model",
				Stream:   true,
				Messages: append([]Message(nil), messages...),
			}
			delay := startMs + int64(turn)*int64(opts.TurnGapMs) + int64(rng.IntN(50))
			reqs = append(reqs, Request{
				SessionID: sid,
				DelayMs:   delay,
				Pattern:   pattern,
				Body:      body,
			})

			// Add a (synthetic) assistant reply so the next turn's prompt
			// extends the history naturally.
			messages = append(messages,
				Message{Role: "assistant", Content: fmt.Sprintf("ack t%d", turn)},
			)
		}
	}
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].DelayMs < reqs[j].DelayMs })
	return Trace{Requests: reqs}
}

// WriteJSONL writes one Request per line to w.
func WriteJSONL(t Trace, w io.Writer) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, r := range t.Requests {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ReadJSONL reads a trace previously emitted by WriteJSONL.
func ReadJSONL(r io.Reader) (Trace, error) {
	var out Trace
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				break
			}
			return Trace{}, err
		}
		out.Requests = append(out.Requests, req)
	}
	return out, nil
}

// Stats summarises a trace's gross shape; useful in reports.
type Stats struct {
	Requests         int
	Sessions         int
	UniqueSysPrompts int
	MaxDelay         time.Duration
	MeanContentChars int
	PatternHistogram map[string]int
}

// Shape computes Stats describing the gross structure of a trace (request
// count, session count, prompt sizes). It is unrelated to the result-level
// Summarise function in report.go.
func Shape(t Trace) Stats {
	st := Stats{PatternHistogram: map[string]int{}}
	st.Requests = len(t.Requests)
	sessions := map[string]struct{}{}
	systems := map[string]struct{}{}
	totalChars := 0
	maxDelayMs := int64(0)
	for _, r := range t.Requests {
		sessions[r.SessionID] = struct{}{}
		st.PatternHistogram[r.Pattern]++
		if r.DelayMs > maxDelayMs {
			maxDelayMs = r.DelayMs
		}
		for _, m := range r.Body.Messages {
			totalChars += len(m.Content)
			if m.Role == "system" {
				systems[m.Content] = struct{}{}
			}
		}
	}
	st.Sessions = len(sessions)
	st.UniqueSysPrompts = len(systems)
	st.MaxDelay = time.Duration(maxDelayMs) * time.Millisecond
	if st.Requests > 0 {
		st.MeanContentChars = totalChars / st.Requests
	}
	return st
}

// newRng returns a deterministic *rand.Rand seeded from a 64-bit seed (or a
// fixed value when seed is 0). math/rand/v2's *rand.Rand does not require
// locking for single-goroutine use, which is all the generator needs.
func newRng(seed int64) *rand.Rand {
	if seed == 0 {
		seed = 1
	}
	return rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))
}

// makeFixedString returns a stable string of approx targetLen built from the
// repeated prefix and a deterministic filler. The same prefix and filler
// produce byte-equal output across runs - critical so different sessions
// share a real prefix.
func makeFixedString(prefix string, targetLen int, filler byte) string {
	if targetLen <= 0 {
		return prefix
	}
	if len(prefix) >= targetLen {
		return prefix
	}
	var b strings.Builder
	b.Grow(targetLen)
	b.WriteString(prefix)
	pad := targetLen - len(prefix)
	for i := 0; i < pad; i++ {
		b.WriteByte(filler)
	}
	return b.String()
}

// makeRandomString returns a non-shared string of length n using a per-call
// filler byte, so different turns vary at the byte level (no spurious tree
// matches between unrelated turns).
func makeRandomString(rng *rand.Rand, n int, filler byte) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		// Mix a deterministic varying byte with a fixed filler so output
		// changes between calls but is reproducible given the same RNG.
		b.WriteByte(filler ^ byte(rng.IntN(64)))
	}
	return b.String()
}
