package trace

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestGenerate_Deterministic(t *testing.T) {
	opts := Default()
	a := Generate(opts)
	b := Generate(opts)
	var ba, bb bytes.Buffer
	if err := WriteJSONL(a, &ba); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONL(b, &bb); err != nil {
		t.Fatal(err)
	}
	if ba.String() != bb.String() {
		t.Errorf("non-deterministic generation: outputs differ")
	}
}

func TestGenerate_DifferentSeedsProduceDifferentTraces(t *testing.T) {
	opts := Default()
	opts.Seed = 1
	a := Generate(opts)
	opts.Seed = 2
	b := Generate(opts)
	var ba, bb bytes.Buffer
	WriteJSONL(a, &ba)
	WriteJSONL(b, &bb)
	if ba.String() == bb.String() {
		t.Fatal("seed change had no effect")
	}
}

func TestGenerate_ZeroAndNegativeKnobsCoerced(t *testing.T) {
	opts := Options{
		Sessions:             0,
		TurnsPerSession:      0,
		UserTurnLen:          0,
		SharedSystemLen:      0,
		Seed:                 0,
		SessionStartJitterMs: 1,
	}
	tr := Generate(opts)
	if len(tr.Requests) == 0 {
		t.Errorf("expected at least one request after coercion")
	}
}

func TestGenerate_RequestsSortedByDelay(t *testing.T) {
	tr := Generate(Default())
	for i := 1; i < len(tr.Requests); i++ {
		if tr.Requests[i].DelayMs < tr.Requests[i-1].DelayMs {
			t.Fatalf("req %d delay %d < %d", i, tr.Requests[i].DelayMs, tr.Requests[i-1].DelayMs)
		}
	}
}

func TestGenerate_SessionsShareSystemPrompt(t *testing.T) {
	tr := Generate(Default())
	systems := map[string]struct{}{}
	for _, r := range tr.Requests {
		for _, m := range r.Body.Messages {
			if m.Role == "system" {
				systems[m.Content] = struct{}{}
			}
		}
	}
	// Two distinct system prompts: the base (always) and the code-context
	// (some sessions). So we expect 2 unique system contents.
	if len(systems) != 2 {
		t.Errorf("expected 2 unique system prompts, got %d", len(systems))
	}
}

func TestGenerate_HistoryGrowsWithinSession(t *testing.T) {
	tr := Generate(Default())
	prevLen := map[string]int{}
	for _, r := range tr.Requests {
		l := len(r.Body.Messages)
		if last, ok := prevLen[r.SessionID]; ok && l <= last {
			t.Errorf("session %s: turn message count %d should be > %d", r.SessionID, l, last)
		}
		prevLen[r.SessionID] = l
	}
}

func TestWriteAndReadJSONL_RoundTrip(t *testing.T) {
	tr := Generate(Default())
	var buf bytes.Buffer
	if err := WriteJSONL(tr, &buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadJSONL(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != len(tr.Requests) {
		t.Fatalf("len mismatch: %d vs %d", len(got.Requests), len(tr.Requests))
	}
	if got.Requests[0].SessionID != tr.Requests[0].SessionID {
		t.Errorf("session id mismatch")
	}
}

func TestReadJSONL_BadInput(t *testing.T) {
	if _, err := ReadJSONL(strings.NewReader("not json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestShape_BasicShape(t *testing.T) {
	tr := Generate(Default())
	st := Shape(tr)
	if st.Requests != len(tr.Requests) {
		t.Errorf("Requests = %d", st.Requests)
	}
	if st.Sessions <= 0 {
		t.Errorf("Sessions = %d", st.Sessions)
	}
	if st.MaxDelay <= 0 {
		t.Errorf("MaxDelay = %v", st.MaxDelay)
	}
	if st.MeanContentChars <= 0 {
		t.Errorf("MeanContentChars = %d", st.MeanContentChars)
	}
	if len(st.PatternHistogram) == 0 {
		t.Errorf("PatternHistogram empty")
	}
}

func TestMakeFixedString(t *testing.T) {
	out := makeFixedString("hi-", 10, 'x')
	if len(out) != 10 {
		t.Errorf("len = %d", len(out))
	}
	if !strings.HasPrefix(out, "hi-") {
		t.Errorf("missing prefix: %q", out)
	}
	if got := makeFixedString("longer-than-target", 5, 'x'); got != "longer-than-target" {
		t.Errorf("expected pass-through when prefix >= target, got %q", got)
	}
	if got := makeFixedString("p", 0, 'x'); got != "p" {
		t.Errorf("zero-target should return prefix verbatim, got %q", got)
	}
}

func TestMakeRandomString_Reproducible(t *testing.T) {
	a := makeRandomString(newRng(123), 32, 'a')
	b := makeRandomString(newRng(123), 32, 'a')
	if a != b {
		t.Errorf("not reproducible: %q vs %q", a, b)
	}
	c := makeRandomString(newRng(124), 32, 'a')
	if c == a {
		t.Errorf("seed 123 vs 124 should differ")
	}
	if got := makeRandomString(newRng(1), 0, 'a'); got != "" {
		t.Errorf("zero-len should be empty, got %q", got)
	}
}

func TestShape_TimeFieldFormat(t *testing.T) {
	st := Shape(Generate(Default()))
	if st.MaxDelay > time.Hour {
		t.Errorf("MaxDelay surprisingly large: %v", st.MaxDelay)
	}
}
