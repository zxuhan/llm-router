package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCircuitBreaker_Defaults(t *testing.T) {
	c := NewCircuitBreaker(0, 0)
	st := c.Snapshot()
	if st.Threshold != 5 || st.Cooldown != 30*time.Second {
		t.Errorf("expected default 5/30s, got %+v", st)
	}
}

func TestCircuitBreaker_TripsAfterThreshold(t *testing.T) {
	c := NewCircuitBreaker(3, 50*time.Millisecond)
	for i := 0; i < 2; i++ {
		c.RecordFailure()
		if !c.Allow() {
			t.Fatalf("breaker tripped early at i=%d", i)
		}
	}
	c.RecordFailure() // third failure trips it
	if c.Allow() {
		t.Fatal("expected breaker to be open after 3 failures")
	}
	if c.Snapshot().Trips != 1 {
		t.Errorf("Trips = %d, want 1", c.Snapshot().Trips)
	}
}

func TestCircuitBreaker_AutoResetAfterCooldown(t *testing.T) {
	c := NewCircuitBreaker(2, 30*time.Millisecond)
	c.RecordFailure()
	c.RecordFailure()
	if c.Allow() {
		t.Fatal("expected open after threshold")
	}
	time.Sleep(40 * time.Millisecond)
	if !c.Allow() {
		t.Fatal("expected closed after cooldown")
	}
	st := c.Snapshot()
	if st.Open || st.Failures != 0 {
		t.Errorf("expected reset state, got %+v", st)
	}
}

func TestCircuitBreaker_SuccessClearsFailures(t *testing.T) {
	c := NewCircuitBreaker(3, time.Second)
	c.RecordFailure()
	c.RecordFailure()
	c.RecordSuccess()
	if got := c.Snapshot().Failures; got != 0 {
		t.Errorf("Failures after RecordSuccess = %d, want 0", got)
	}
	// New failure run should not trip yet (counter was reset).
	c.RecordFailure()
	c.RecordFailure()
	if !c.Allow() {
		t.Fatal("breaker should still be closed; counter was reset")
	}
}

func TestCircuitBreaker_SuccessClosesOpenBreaker(t *testing.T) {
	c := NewCircuitBreaker(2, time.Hour) // long cooldown so we don't auto-close
	c.RecordFailure()
	c.RecordFailure()
	if c.Allow() {
		t.Fatal("expected open")
	}
	c.RecordSuccess()
	if !c.Allow() {
		t.Fatal("expected RecordSuccess to immediately close the breaker")
	}
}

func TestCircuitBreaker_ConcurrentSafe(t *testing.T) {
	c := NewCircuitBreaker(50, 10*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				switch j % 3 {
				case 0:
					_ = c.Allow()
				case 1:
					c.RecordFailure()
				case 2:
					c.RecordSuccess()
				}
			}
		}()
	}
	wg.Wait()
	// No race detected; final state is reachable.
	_ = c.Snapshot()
}

func TestLlamaCpp_BreakerTripsOn5xxAndClosesOn2xx(t *testing.T) {
	var failNext int32
	var srvHandler http.HandlerFunc = func(w http.ResponseWriter, _ *http.Request) {
		if failNext > 0 {
			failNext--
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	srv := httptest.NewServer(srvHandler)
	defer srv.Close()

	b, err := NewLlamaCpp(LlamaCppOptions{
		ID: "w0", URL: srv.URL,
		CircuitBreakerThreshold: 3, CircuitBreakerCooldown: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Healthy() {
		t.Fatal("expected fresh backend to be healthy")
	}

	// Three 5xx in a row should trip the breaker.
	failNext = 3
	for i := 0; i < 3; i++ {
		resp, _ := b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	if b.Healthy() {
		t.Fatal("expected breaker to be open after 3 failures")
	}

	// After cooldown the breaker auto-resets.
	time.Sleep(60 * time.Millisecond)
	if !b.Healthy() {
		t.Fatal("expected breaker to auto-close after cooldown")
	}

	// A successful call should keep things healthy.
	resp, err := b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !b.Healthy() {
		t.Fatal("expected healthy after successful 2xx")
	}
}

func TestLlamaCpp_4xxDoesNotTripBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()
	b, _ := NewLlamaCpp(LlamaCppOptions{
		ID: "w0", URL: srv.URL,
		CircuitBreakerThreshold: 2, CircuitBreakerCooldown: time.Hour,
	})
	for i := 0; i < 5; i++ {
		resp, _ := b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
		_ = resp.Body.Close()
	}
	if !b.Healthy() {
		t.Fatal("4xx responses should not trip the breaker")
	}
}

func TestLlamaCpp_TransportErrorsTripBreaker(t *testing.T) {
	b, _ := NewLlamaCpp(LlamaCppOptions{
		ID: "w0", URL: "http://127.0.0.1:1",
		CircuitBreakerThreshold: 2, CircuitBreakerCooldown: time.Hour,
	})
	for i := 0; i < 2; i++ {
		_, _ = b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
	}
	if b.Healthy() {
		t.Fatal("expected breaker to be open after transport errors")
	}
	if got := b.CircuitState().Trips; got == 0 {
		t.Errorf("expected at least one Trips, got %d", got)
	}
}
