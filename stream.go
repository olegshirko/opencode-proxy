package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pingWriter wraps the client response. Until Commit it behaves like a normal
// writer; after Commit it has already sent 200 + text/event-stream and emits
// `event: ping` on an interval, so a client waiting through retries sees a
// live stream instead of silence.
type pingWriter struct {
	inner      http.ResponseWriter
	flusher    http.Flusher
	interval   time.Duration
	keepalives *atomic.Int64

	mu        sync.Mutex
	committed bool
	stop      chan struct{}
	done      chan struct{}
}

func newPingWriter(w http.ResponseWriter, interval time.Duration, keepalives *atomic.Int64) *pingWriter {
	pw := &pingWriter{inner: w, interval: interval, keepalives: keepalives}
	if f, ok := w.(http.Flusher); ok {
		pw.flusher = f
	}
	return pw
}

func (pw *pingWriter) Header() http.Header { return pw.inner.Header() }

func (pw *pingWriter) WriteHeader(status int) { pw.inner.WriteHeader(status) }

func (pw *pingWriter) Write(b []byte) (int, error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.inner.Write(b)
}

func (pw *pingWriter) Flush() {
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
}

func (pw *pingWriter) Committed() bool {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.committed
}

// Commit sends the SSE preamble once and starts the ping loop.
func (pw *pingWriter) Commit() {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.committed {
		return
	}
	pw.committed = true
	h := pw.inner.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	pw.inner.WriteHeader(http.StatusOK)
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
	pw.stop = make(chan struct{})
	pw.done = make(chan struct{})
	go pw.pingLoop(pw.stop, pw.done)
}

func (pw *pingWriter) pingLoop(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(pw.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			pw.mu.Lock()
			_, err := fmt.Fprint(pw.inner, "event: ping\ndata: {}\n\n")
			if err == nil {
				if pw.flusher != nil {
					pw.flusher.Flush()
				}
				if pw.keepalives != nil {
					pw.keepalives.Add(1)
				}
			}
			pw.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// StopPings ends the ping loop and waits for it to exit. Safe to call twice.
// Writes through this writer never interleave with a ping.
func (pw *pingWriter) StopPings() {
	pw.mu.Lock()
	stop, done := pw.stop, pw.done
	pw.stop, pw.done = nil, nil
	pw.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// serveStream forwards a streaming request, retrying 429/5xx. If the upstream
// stays silent past keepalive_after_ms, or answers with a retryable status,
// the client is switched to a committed SSE stream and receives pings until
// the real response (or a terminal error event) arrives.
func (p *Proxy) serveStream(ctx context.Context, w http.ResponseWriter, cfg *Config, base *http.Request, body []byte, modelIn string, capture *CaptureRecord, cell string) (int, int, usageAccumulator, bool) {
	kaAfter := time.Duration(cfg.Scheduler.KeepaliveAfterMS) * time.Millisecond
	kaInterval := time.Duration(cfg.Scheduler.KeepaliveIntervalMS) * time.Millisecond
	if kaInterval <= 0 {
		kaInterval = 5 * time.Second
	}
	policy := NewRetryPolicy(cfg.Retry, nil)
	budget := time.Duration(cfg.Retry.TotalBudgetMS) * time.Millisecond
	maxAttempts := cfg.Retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	pw := newPingWriter(w, kaInterval, &p.keepalives)
	defer pw.StopPings()

	var waited time.Duration
	var acc usageAccumulator

	for attempt := 1; ; attempt++ {
		type result struct {
			resp *http.Response
			err  error
		}
		ch := make(chan result, 1)
		req := cloneRequestBody(ctx, base, body)
		go func() {
			resp, err := p.client.Do(req)
			ch <- result{resp, err}
		}()

		var out result
		if !pw.Committed() {
			timer := time.NewTimer(kaAfter)
			select {
			case out = <-ch:
				timer.Stop()
			case <-timer.C:
				// Upstream is taking its time: keep the client's stream alive.
				pw.Commit()
				out = <-ch
			case <-ctx.Done():
				timer.Stop()
				return 499, attempt, acc, true
			}
		} else {
			select {
			case out = <-ch:
			case <-ctx.Done():
				return 499, attempt, acc, true
			}
		}

		if out.err != nil {
			if ctx.Err() != nil {
				return 499, attempt, acc, true
			}
			if !pw.Committed() && attempt >= maxAttempts {
				http.Error(w, out.err.Error(), http.StatusBadGateway)
				return http.StatusBadGateway, attempt, acc, false
			}
			if attempt >= maxAttempts {
				pw.StopPings()
				writeSSEError(w, []byte(`{"type":"error","error":{"type":"api_error","message":"upstream unreachable"}}`))
				return http.StatusBadGateway, attempt, acc, false
			}
			continue
		}

		resp := out.resp
		if resp.StatusCode == http.StatusTooManyRequests {
			p.upstream429.Add(1)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.sched.ReportSuccess(cell)
			pw.StopPings()
			if capture != nil && cfg.Log.Capture.ResponseHeaders {
				capture.ResponseHeaders = maskHeaders(resp.Header)
			}
			if !pw.Committed() {
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
			}
			defer resp.Body.Close()
			acc, aborted := relayResponse(ctx, pw, resp, capture, cfg, modelIn)
			return resp.StatusCode, attempt, acc, aborted
		}

		retryable := ShouldRetryResponse(resp)
		if retryable {
			p.sched.ReportFailure(cell)
		}
		var wait time.Duration
		if retryable {
			wait = policy.WaitFor(attempt, parseRetryAfter(resp.Header.Get("Retry-After")))
		}
		if !retryable || attempt >= maxAttempts || waited+wait > budget {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if !pw.Committed() {
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(bodyBytes)
				return resp.StatusCode, attempt, acc, false
			}
			pw.StopPings()
			writeSSEError(w, bodyBytes)
			return resp.StatusCode, attempt, acc, false
		}

		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		p.retries.Add(1)
		// The client is told something is happening before the first wait.
		if !pw.Committed() {
			pw.Commit()
		}
		if !sleepCtx(ctx, wait) {
			return 499, attempt, acc, true
		}
		waited += wait
	}
}

// writeSSEError delivers a terminal error inside an already-committed stream.
func writeSSEError(w http.ResponseWriter, body []byte) {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	msg := `{"type":"api_error","message":"upstream request failed"}`
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		msg = string(envelope.Error)
	}
	fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":%s}\n\n", msg)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
