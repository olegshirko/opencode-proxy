package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tokenCacheTTL  = 60 * time.Second
	maxSessions    = 1000
	copyBufferSize = 32 * 1024
)

// Stats is the counter snapshot served by the admin API.
type Stats struct {
	Requests    int64
	Retries     int64
	Upstream429 int64
	InFlight    int64
}

// SessionCost is the accumulated cost of one session. USD sums
// usage.opencode.allowance and ListUSD sums usage.opencode.list_cost.
type SessionCost struct {
	Session  string               `json:"session"`
	USD      float64              `json:"usd"`
	ListUSD  float64              `json:"list_usd"`
	Requests int                  `json:"requests"`
	FirstAt  time.Time            `json:"first_at"`
	LastAt   time.Time            `json:"last_at"`
	ByModel  map[string]ModelCost `json:"by_model"`
}

type ModelCost struct {
	USD      float64 `json:"usd"`
	ListUSD  float64 `json:"list_usd"`
	Requests int     `json:"requests"`
}

// Proxy is the HTTP handler that forwards Anthropic API calls to the OpenCode
// gateway.
type Proxy struct {
	cfg    *ConfigLoader
	reg    *Registry
	log    *Logger
	client *http.Client

	requests    atomic.Int64
	retries     atomic.Int64
	upstream429 atomic.Int64
	inFlight    atomic.Int64

	tokens tokenCache

	costs *CostStore

	// onRecord observes every logged record. It is a test seam; production
	// code leaves it nil.
	onRecord func(RequestRecord)
}

func NewProxy(cfg *ConfigLoader, reg *Registry, log *Logger, client *http.Client) *Proxy {
	if client == nil {
		client = &http.Client{}
	}
	store, _ := NewCostStore("")
	return &Proxy{
		cfg:    cfg,
		reg:    reg,
		log:    log,
		client: client,
		tokens: newTokenCache(),
		costs:  store,
	}
}

// SetCostStore swaps the cost store, which main uses to give the proxy a
// persistent one.
func (p *Proxy) SetCostStore(s *CostStore) {
	p.costs = s
}

// Stats returns the current counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		Requests:    p.requests.Load(),
		Retries:     p.retries.Load(),
		Upstream429: p.upstream429.Load(),
		InFlight:    p.inFlight.Load(),
	}
}

// SessionCost returns a copy of the cost record for session. An unknown
// session yields a zero record rather than an error.
func (p *Proxy) SessionCost(session string) SessionCost {
	return p.costs.Get(session)
}

func (p *Proxy) addSessionCost(session, model string, allowance, listCost float64, now time.Time) {
	p.costs.Add(session, model, allowance, listCost, now)
}

var requestIDCounter atomic.Uint64

func newRequestID() string {
	return fmt.Sprintf("%06x", requestIDCounter.Add(1)&0xffffff)
}

// forwardedHeaders are the client headers copied to the upstream request.
var forwardedHeaders = []string{
	"Authorization", "x-api-key", "anthropic-version",
	"content-type", "user-agent", "anthropic-beta",
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p.requests.Add(1)
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)

	cfg := p.cfg.Get()
	session := r.Header.Get("X-Claude-Code-Session-Id")
	if session == "" {
		session = cfg.SessionFallback
	}

	var (
		modelIn, modelOut, why string
		capture                *CaptureRecord
		acc                    usageAccumulator
		status, attempts       int
		aborted, billable      = false, true
	)

	defer func() {
		if billable && !aborted && status >= 200 && status < 300 {
			model := modelOut
			if model == "" {
				model = modelIn
			}
			p.addSessionCost(session, model, acc.allowance, acc.cost, time.Now())
		}
		rec := RequestRecord{
			ID: newRequestID(), Method: r.Method, Path: r.URL.Path, Session: session,
			ModelIn: modelIn, ModelOut: modelOut, Why: why,
			Status: status, Attempt: attempts, Duration: time.Since(start),
			In: acc.in, Out: acc.out, CacheRead: acc.cache,
			Cost: acc.cost, Allowance: acc.allowance,
			Aborted: aborted, Capture: capture,
		}
		if p.onRecord != nil {
			p.onRecord(rec)
		}
		if p.log != nil {
			p.log.Request(rec)
		}
	}()

	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			if aborted = r.Context().Err() != nil; !aborted {
				http.Error(w, err.Error(), http.StatusBadRequest)
				status = http.StatusBadRequest
			}
			return
		}
	}

	if len(body) > 0 && json.Valid(body) {
		var data map[string]any
		if json.Unmarshal(body, &data) == nil {
			if m, ok := data["model"].(string); ok {
				target, reason := p.reg.Resolve(m, cfg.Models)
				data["model"] = target
				if rewritten, err := json.Marshal(data); err == nil {
					body = rewritten
					modelIn, modelOut, why = m, target, reason
				}
			}
		}
	}

	ctx := r.Context()
	base, err := p.upstreamRequest(ctx, r, cfg, body, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		status = http.StatusBadGateway
		return
	}
	capture = newCaptureRecord(cfg, base.Header, body)

	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens" {
		billable = false
		res, err := p.countTokens(ctx, cfg, base, body)
		if err != nil {
			if aborted = ctx.Err() != nil; !aborted {
				http.Error(w, err.Error(), http.StatusBadGateway)
				status = http.StatusBadGateway
			}
			return
		}
		status, attempts = res.status, res.attempts
		if capture != nil && cfg.Log.Capture.ResponseHeaders {
			capture.ResponseHeaders = maskHeaders(res.header)
		}
		copyHeaders(w.Header(), res.header)
		w.WriteHeader(res.status)
		_, werr := w.Write(res.body)
		fillCapturedBody(capture, cfg, res.body)
		if werr != nil && ctx.Err() != nil {
			aborted = true
		}
		return
	}

	resp, n, err := p.doAttempts(ctx, cfg, func() (*http.Request, error) {
		return cloneRequestBody(ctx, base, body), nil
	})
	attempts = n
	if err != nil {
		if aborted = ctx.Err() != nil; !aborted {
			http.Error(w, err.Error(), http.StatusBadGateway)
			status = http.StatusBadGateway
		}
		return
	}
	defer resp.Body.Close()

	status = resp.StatusCode
	if capture != nil && cfg.Log.Capture.ResponseHeaders {
		capture.ResponseHeaders = maskHeaders(resp.Header)
	}
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	acc, aborted = relayResponse(ctx, w, resp, capture, cfg)
}

func (p *Proxy) upstreamRequest(ctx context.Context, r *http.Request, cfg *Config, body []byte, session string) (*http.Request, error) {
	url := strings.TrimRight(cfg.Upstream, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, name := range forwardedHeaders {
		for _, v := range r.Header.Values(name) {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("x-opencode-session", session)
	return req, nil
}

// cloneRequestBody returns a fresh copy of base with a new body reader,
// because an *http.Request body can only be consumed once.
func cloneRequestBody(ctx context.Context, base *http.Request, body []byte) *http.Request {
	req := base.Clone(ctx)
	if len(body) == 0 {
		req.Body = http.NoBody
		req.ContentLength = 0
		return req
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return req
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// doAttempts forwards one request, retrying 429 and 5xx responses until the
// attempt count or the total wait budget is exhausted. It returns the final
// response (whose headers have not been written to the client yet) and the
// number of attempts made.
func (p *Proxy) doAttempts(ctx context.Context, cfg *Config, newReq func() (*http.Request, error)) (*http.Response, int, error) {
	policy := NewRetryPolicy(cfg.Retry, nil)
	maxAttempts := cfg.Retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	budget := time.Duration(cfg.Retry.TotalBudgetMS) * time.Millisecond
	var waited time.Duration

	for attempt := 1; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, attempt, err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, attempt, err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			p.upstream429.Add(1)
		}
		if !ShouldRetry(resp.StatusCode) || attempt >= maxAttempts {
			return resp, attempt, nil
		}

		// Retry-After is the only delay signal from upstream. The prose hint
		// in the body (ParsePrefillHint) is deliberately not used here.
		wait := policy.WaitFor(attempt, parseRetryAfter(resp.Header.Get("Retry-After")))
		if waited+wait > budget {
			return resp, attempt, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if !sleepCtx(ctx, wait) {
			return nil, attempt, ctx.Err()
		}
		waited += wait
		p.retries.Add(1)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// parseRetryAfter decodes a Retry-After header given as seconds or an
// HTTP-date. It returns 0 when the value is absent, invalid or in the past.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// relayResponse streams the upstream body to the client, flushing every
// chunk, while parsing usage for the request log and capturing the body when
// requested. It reports whether the client went away.
func relayResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *CaptureRecord, cfg *Config) (usageAccumulator, bool) {
	var (
		parser   *sseParser
		raw      bytes.Buffer
		captured *limitedBuffer
		acc      usageAccumulator
	)
	if isEventStream(resp.Header) {
		parser = &sseParser{}
	}
	if rec != nil && cfg.Log.Capture.ResponseBody {
		captured = &limitedBuffer{max: cfg.Log.MaxBodyBytes}
	}

	buf := make([]byte, copyBufferSize)
	aborted := false
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if parser != nil {
				parser.Feed(chunk)
			} else {
				raw.Write(chunk)
			}
			if captured != nil {
				captured.Write(chunk)
			}
			if _, werr := w.Write(chunk); werr != nil {
				aborted = true
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() != nil {
				aborted = true
			}
			break
		}
	}

	if parser != nil {
		parser.Close()
		acc = parser.acc
	} else {
		acc = parseUsageJSON(raw.Bytes())
	}
	if captured != nil {
		rec.ResponseBody = captured.String()
		if captured.truncated {
			rec.Truncated = true
		}
	}
	return acc, aborted
}

func isEventStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// sseParser reconstructs SSE lines from arbitrary read chunks: bytes after
// the last newline stay pending until the next chunk completes the line.
type sseParser struct {
	pending []byte
	acc     usageAccumulator
}

func (s *sseParser) Feed(chunk []byte) {
	s.pending = append(s.pending, chunk...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			return
		}
		s.processLine(s.pending[:i])
		s.pending = s.pending[i+1:]
	}
}

func (s *sseParser) Close() {
	if len(s.pending) > 0 {
		s.processLine(s.pending)
		s.pending = nil
	}
}

func (s *sseParser) processLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload := bytes.TrimSpace(rest)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var env usageEnvelope
	if json.Unmarshal(payload, &env) != nil {
		return
	}
	env.observe(&s.acc)
}

// usageEnvelope accepts both the non-streaming shape (usage at the top level)
// and the message_start shape (usage under message).
type usageEnvelope struct {
	Usage   *usageInfo `json:"usage"`
	Message *struct {
		Usage *usageInfo `json:"usage"`
	} `json:"message"`
}

func (e *usageEnvelope) observe(a *usageAccumulator) {
	if e.Message != nil {
		a.observe(e.Message.Usage)
	}
	a.observe(e.Usage)
}

type usageInfo struct {
	InputTokens          *float64       `json:"input_tokens"`
	OutputTokens         *float64       `json:"output_tokens"`
	CacheReadInputTokens *float64       `json:"cache_read_input_tokens"`
	Opencode             *opencodeUsage `json:"opencode"`
}

type opencodeUsage struct {
	ListCost  float64         `json:"list_cost"`
	Allowance float64         `json:"allowance"`
	Tokens    *opencodeTokens `json:"tokens"`
}

type opencodeTokens struct {
	Input     *float64 `json:"input"`
	Output    *float64 `json:"output"`
	CacheRead *float64 `json:"cache_read"`
}

// usageAccumulator keeps the last value seen for each field. The gateway
// repeats usage.opencode in message_start and message_delta; those describe
// one request, so they are overwritten, never summed.
type usageAccumulator struct {
	in, out, cache    float64
	cost, allowance   float64
	haveIn, haveCache bool
}

func (a *usageAccumulator) observe(u *usageInfo) {
	if u == nil {
		return
	}
	if oc := u.Opencode; oc != nil {
		a.cost = oc.ListCost
		a.allowance = oc.Allowance
		if t := oc.Tokens; t != nil {
			a.setIn(t.Input)
			a.setCache(t.CacheRead)
			if t.Output != nil {
				a.out = *t.Output
			}
		}
	}
	a.setIn(u.InputTokens)
	a.setCache(u.CacheReadInputTokens)
	if u.OutputTokens != nil {
		a.out = *u.OutputTokens
	}
}

// setIn and setCache accept a zero only when no earlier payload provided a
// value, so a message_delta that repeats input_tokens: 0 cannot erase the
// counts reported by message_start.
func (a *usageAccumulator) setIn(v *float64) {
	if v != nil && (*v != 0 || !a.haveIn) {
		a.in, a.haveIn = *v, true
	}
}

func (a *usageAccumulator) setCache(v *float64) {
	if v != nil && (*v != 0 || !a.haveCache) {
		a.cache, a.haveCache = *v, true
	}
}

func parseUsageJSON(body []byte) usageAccumulator {
	var (
		env usageEnvelope
		acc usageAccumulator
	)
	if json.Unmarshal(body, &env) != nil {
		return acc
	}
	env.observe(&acc)
	return acc
}

func newCaptureRecord(cfg *Config, header http.Header, body []byte) *CaptureRecord {
	cc := cfg.Log.Capture
	if !cc.RequestHeaders && !cc.RequestBody && !cc.ResponseHeaders && !cc.ResponseBody {
		return nil
	}
	rec := &CaptureRecord{}
	if cc.RequestHeaders {
		rec.RequestHeaders = maskHeaders(header)
	}
	if cc.RequestBody {
		rec.RequestBody, rec.Truncated = truncateBody(body, cfg.Log.MaxBodyBytes)
	}
	return rec
}

func fillCapturedBody(rec *CaptureRecord, cfg *Config, body []byte) {
	if rec == nil || !cfg.Log.Capture.ResponseBody {
		return
	}
	text, truncated := truncateBody(body, cfg.Log.MaxBodyBytes)
	rec.ResponseBody = text
	if truncated {
		rec.Truncated = true
	}
}

// truncateBody cuts b to max bytes. A non-positive max means no limit.
func truncateBody(b []byte, max int64) (string, bool) {
	if max > 0 && int64(len(b)) > max {
		return string(b[:max]), true
	}
	return string(b), false
}

// limitedBuffer keeps at most max bytes and records that more arrived.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.max > 0 {
		remain := l.max - int64(l.buf.Len())
		if remain < int64(len(p)) {
			if remain > 0 {
				l.buf.Write(p[:remain])
			}
			l.truncated = true
			return len(p), nil
		}
	}
	l.buf.Write(p)
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.buf.String() }

// cachedResponse is one upstream count_tokens reply kept for reuse.
type cachedResponse struct {
	status   int
	header   http.Header
	body     []byte
	attempts int
}

type tokenCacheEntry struct {
	resp    *cachedResponse
	expires time.Time
}

type tokenInflight struct {
	done chan struct{}
	resp *cachedResponse
	err  error
}

// tokenCache caches count_tokens replies by body hash for 60 seconds and
// collapses concurrent misses for the same body into a single upstream call.
type tokenCache struct {
	mu       sync.Mutex
	entries  map[string]tokenCacheEntry
	inflight map[string]*tokenInflight
}

func newTokenCache() tokenCache {
	return tokenCache{
		entries:  make(map[string]tokenCacheEntry),
		inflight: make(map[string]*tokenInflight),
	}
}

func (c *tokenCache) getOrLoad(ctx context.Context, key string, load func() (*cachedResponse, error)) (*cachedResponse, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if time.Now().Before(e.expires) {
			c.mu.Unlock()
			return e.resp, nil
		}
		delete(c.entries, key)
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.resp, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &tokenInflight{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	resp, err := load()

	c.mu.Lock()
	call.resp, call.err = resp, err
	if err == nil && resp != nil && resp.status >= 200 && resp.status < 300 {
		now := time.Now()
		c.entries[key] = tokenCacheEntry{resp: resp, expires: now.Add(tokenCacheTTL)}
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	delete(c.inflight, key)
	close(call.done)
	c.mu.Unlock()
	return resp, err
}

func tokenCacheKey(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (p *Proxy) countTokens(ctx context.Context, cfg *Config, base *http.Request, body []byte) (*cachedResponse, error) {
	return p.tokens.getOrLoad(ctx, tokenCacheKey(body), func() (*cachedResponse, error) {
		resp, attempts, err := p.doAttempts(ctx, cfg, func() (*http.Request, error) {
			return cloneRequestBody(ctx, base, body), nil
		})
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return &cachedResponse{
			status:   resp.StatusCode,
			header:   resp.Header.Clone(),
			body:     data,
			attempts: attempts,
		}, nil
	})
}
