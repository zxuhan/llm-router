package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Result captures the outcome of replaying one trace request.
type Result struct {
	SessionID   string        `json:"session_id"`
	Pattern     string        `json:"pattern"`
	Index       int           `json:"index"`
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt time.Time     `json:"completed_at"`
	StatusCode  int           `json:"status_code"`
	TTFT        time.Duration `json:"ttft"`
	Total       time.Duration `json:"total"`
	BytesIn     int64         `json:"bytes_in"`
	BackendID   string        `json:"backend_id"`
	Reason      string        `json:"reason"`
	Err         string        `json:"err,omitempty"`
}

// Replayer fires a Trace at a router endpoint with realistic timing. Sessions
// run concurrently; turns within a session are strictly serialised so the
// router sees the same prefix-growth pattern as the original conversation.
type Replayer struct {
	// Endpoint is the full URL of the router's chat-completions handler.
	Endpoint string
	// Client is the HTTP client used for each request. nil uses a streaming-
	// friendly default. The client must NOT have a per-request timeout, or
	// long generations would be terminated mid-stream.
	Client *http.Client
}

// Replay fires every request in t at the configured endpoint and returns one
// Result per request, in the trace's original order.
func (r *Replayer) Replay(ctx context.Context, t Trace) ([]Result, error) {
	if r.Endpoint == "" {
		return nil, fmt.Errorf("replayer: Endpoint is required")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Transport: defaultReplayTransport()}
	}

	start := time.Now()

	// Group requests by session, preserving each request's original index so
	// we can place results back in trace order.
	type indexed struct {
		idx int
		req Request
	}
	bySession := map[string][]indexed{}
	for i, req := range t.Requests {
		bySession[req.SessionID] = append(bySession[req.SessionID], indexed{i, req})
	}
	for _, lst := range bySession {
		sort.SliceStable(lst, func(i, j int) bool { return lst[i].req.DelayMs < lst[j].req.DelayMs })
	}

	results := make([]Result, len(t.Requests))
	var wg sync.WaitGroup
	for sid, list := range bySession {
		wg.Add(1)
		go func(sid string, list []indexed) {
			defer wg.Done()
			for _, item := range list {
				if err := ctx.Err(); err != nil {
					results[item.idx] = Result{
						SessionID: sid,
						Pattern:   item.req.Pattern,
						Index:     item.idx,
						Err:       err.Error(),
					}
					return
				}
				target := start.Add(time.Duration(item.req.DelayMs) * time.Millisecond)
				if d := time.Until(target); d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						results[item.idx] = Result{
							SessionID: sid,
							Pattern:   item.req.Pattern,
							Index:     item.idx,
							Err:       ctx.Err().Error(),
						}
						return
					}
				}
				res := r.fire(ctx, client, item.req, item.idx)
				results[item.idx] = res
			}
		}(sid, list)
	}
	wg.Wait()
	return results, nil
}

// fire performs one HTTP request and returns its Result.
func (r *Replayer) fire(ctx context.Context, client *http.Client, req Request, idx int) Result {
	res := Result{
		SessionID: req.SessionID,
		Pattern:   req.Pattern,
		Index:     idx,
		StartedAt: time.Now(),
	}
	body, err := json.Marshal(req.Body)
	if err != nil {
		res.Err = "marshal: " + err.Error()
		res.CompletedAt = time.Now()
		return res
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint, bytes.NewReader(body))
	if err != nil {
		res.Err = "new request: " + err.Error()
		res.CompletedAt = time.Now()
		return res
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Replay-Session", req.SessionID)

	resp, err := client.Do(httpReq)
	if err != nil {
		res.Err = "do: " + err.Error()
		res.CompletedAt = time.Now()
		return res
	}
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode
	res.BackendID = resp.Header.Get("X-Router-Backend")
	res.Reason = resp.Header.Get("X-Router-Reason")

	// Streaming read: record TTFT on the first non-zero read and the byte total.
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if res.TTFT == 0 {
				res.TTFT = time.Since(res.StartedAt)
			}
			res.BytesIn += int64(n)
		}
		if rerr != nil {
			if rerr != io.EOF {
				res.Err = "read: " + rerr.Error()
			}
			break
		}
	}
	res.CompletedAt = time.Now()
	res.Total = res.CompletedAt.Sub(res.StartedAt)
	return res
}

// defaultReplayTransport returns an http.Transport tuned for keeping a steady
// stream of concurrent connections to a single endpoint. We deliberately do
// not set a Client.Timeout; long generations should not be cut.
func defaultReplayTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
}
