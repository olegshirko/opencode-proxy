package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"opencode-proxy/internal/reqlog"
	"opencode-proxy/internal/retry"
	"opencode-proxy/internal/scheduler"
	"opencode-proxy/internal/thinking"
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
	// countTokensConcurrency is how many token counts may run at once. They
	// are cheap and never prefill, but an unbounded burst would still compete
	// with real requests for the gateway's capacity.
	countTokensConcurrency = 4
)

// Stats is the counter snapshot served by the admin API.
type Stats struct {
	Requests        int64
	Retries         int64
	Upstream429     int64
	PrefillRefusals int64
	InFlight        int64
	Queued          int
	CellsInUse      int
	CellLimit       int
	QueueWaitMS     int64
	Keepalives      int64
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
	log    *reqlog.Logger
	client *http.Client

	requests    atomic.Int64
	retries     atomic.Int64
	upstream429 atomic.Int64
	prefill429  atomic.Int64
	inFlight    atomic.Int64
	keepalives  atomic.Int64

	tokens tokenCache
	// catalog is the gateway's live model list behind /v1/models.
	catalog *liveCatalog
	// countSlots caps concurrent token counting. Counting never prefills a
	// prompt, so it stays out of the cell scheduler, but it still must not
	// flood the gateway when the app asks for many bodies at once.
	countSlots chan struct{}

	costs *CostStore
	usage *UsageLedger

	sched *scheduler.CellScheduler

	// onRecord observes every logged record. It is a test seam; production
	// code leaves it nil.
	onRecord func(reqlog.RequestRecord)
}

func NewProxy(cfg *ConfigLoader, reg *Registry, log *reqlog.Logger, client *http.Client) *Proxy {
	if client == nil {
		client = &http.Client{}
	}
	store, _ := NewCostStore("")
	return &Proxy{
		cfg:        cfg,
		reg:        reg,
		log:        log,
		client:     client,
		tokens:     newTokenCache(),
		catalog:    newLiveCatalog(),
		costs:      store,
		sched:      scheduler.NewCellScheduler(8),
		countSlots: make(chan struct{}, countTokensConcurrency),
	}
}

// SetScheduler swaps the cell scheduler, which main configures from the
// scheduler section of the config.
func (p *Proxy) SetScheduler(s *scheduler.CellScheduler) {
	p.sched = s
}

// SetCostStore swaps the cost store, which main uses to give the proxy a
// persistent one.
func (p *Proxy) SetCostStore(s *CostStore) {
	p.costs = s
}

// SetUsageLedger gives the proxy the spend history used for the quota headers
// Claude Code reads. Without it no quota headers are emitted.
func (p *Proxy) SetUsageLedger(l *UsageLedger) {
	p.usage = l
}

// Quota reports the current spend rolled up into the quota windows.
func (p *Proxy) Quota(limits LimitsConfig, now time.Time) UsageSnapshot {
	if p.usage == nil {
		return UsageSnapshot{}
	}
	return p.usage.Snapshot(limits, now)
}

// Stats returns the current counters.
func (p *Proxy) Stats() Stats {
	active, queued := p.sched.Stats()
	return Stats{
		Requests:        p.requests.Load(),
		Retries:         p.retries.Load(),
		Upstream429:     p.upstream429.Load(),
		PrefillRefusals: p.prefill429.Load(),
		InFlight:        p.inFlight.Load(),
		Queued:          queued,
		CellsInUse:      active,
		CellLimit:       p.sched.Limit(),
		QueueWaitMS:     p.sched.QueueWaitMS(),
		Keepalives:      p.keepalives.Load(),
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

// applyQuota writes the unified rate-limit headers Claude Code reads to show
// remaining allowance. It runs only when a monthly budget is configured.
func (p *Proxy) applyQuota(h http.Header, cfg *Config) {
	if p.usage == nil || cfg.Limits.MonthlyUSD <= 0 {
		return
	}
	p.Quota(cfg.Limits, time.Now()).Apply(h)
}

var requestIDCounter atomic.Uint64

func newRequestID() string {
	return fmt.Sprintf("%06x", requestIDCounter.Add(1)&0xffffff)
}

// forwardedHeaders are the client headers always copied upstream. Every
// anthropic-* header is copied as well, whatever it is: the gateway contract
// requires anthropic-beta and its siblings to reach the provider verbatim,
// otherwise extended context, interleaved thinking, tool search and
// structured outputs are silently disabled.
var forwardedHeaders = []string{
	"Authorization", "x-api-key", "anthropic-version",
	"content-type", "user-agent",
}

func forwardHeader(lower string) bool {
	for _, name := range forwardedHeaders {
		if strings.EqualFold(name, lower) {
			return true
		}
	}
	return strings.HasPrefix(lower, "anthropic-")
}

// cellKey derives the upstream cell for a request. Each agent chain gets its
// own cell so parallel subagents stop refusing each other by prefill.
// Compaction deliberately stays on the main cell: it needs the whole context.
func cellKey(r *http.Request, session string) (key, agent, class string, priority int) {
	agent = r.Header.Get("X-Claude-Code-Agent-Id")
	class = r.Header.Get("X-Claude-Code-Request-Class")
	switch {
	case r.Header.Get("X-Claude-Code-Compaction") != "":
		return "s:" + session, agent, class, scheduler.PriorityMain
	case strings.EqualFold(class, "auxiliary"):
		return "s:" + session + ":aux", agent, class, scheduler.PriorityAuxiliary
	case agent != "":
		return "s:" + session + ":a:" + agent, agent, class, scheduler.PrioritySubagent
	default:
		return "s:" + session, agent, class, scheduler.PriorityMain
	}
}

// writeOverloaded emits an Anthropic-shaped overloaded_error body.
func writeOverloaded(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "overloaded_error",
			"message": message,
		},
	})
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
	cell, agent, class, prio := cellKey(r, session)
	// A compaction rewrites the prompt prefix: any rotated key cached for this
	// session points at a stale prefill, so the chain starts from the base key.
	if r.Header.Get("X-Claude-Code-Compaction") != "" {
		p.sched.ResetCell(cell)
	}
	cell = p.sched.EffectiveKey(cell, prio, cfg.Scheduler.FallbackRotateMain,
		time.Duration(cfg.Scheduler.FallbackAfterMS)*time.Millisecond)
	var waited time.Duration

	var (
		modelIn, modelOut, why string
		capture                *reqlog.CaptureRecord
		acc                    usageAccumulator
		status, attempts       int
		aborted, billable      = false, true
		streamReq              bool
	)

	defer func() {
		if billable && !aborted && status >= 200 && status < 300 {
			model := modelOut
			if model == "" {
				model = modelIn
			}
			now := time.Now()
			p.addSessionCost(session, model, acc.allowance, acc.cost, now)
			// The quota ledger tracks what the plan actually charged, which is
			// allowance, not the list price.
			p.usage.Add(model, acc.allowance, now)
		}
		rec := reqlog.RequestRecord{
			ID: newRequestID(), Method: r.Method, Path: r.URL.Path, Session: session,
			ModelIn: modelIn, ModelOut: modelOut, Why: why,
			Status: status, Attempt: attempts, Duration: time.Since(start),
			In: acc.in, Out: acc.out, CacheRead: acc.cache,
			Cost: acc.cost, Allowance: acc.allowance,
			Aborted: aborted, Capture: capture,
			Cell: cell, Agent: agent, Class: class, QueueMS: waited.Milliseconds(),
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
			if v, ok := data["stream"].(bool); ok && v {
				streamReq = true
			}
			thinking.StripSummaries(data)
			if m, ok := data["model"].(string); ok {
				target, reason := p.reg.Resolve(m, cfg.Models)
				// A model the gateway added after the last snapshot is not in
				// the registry; the live catalog resolves it instead of letting
				// it fall through to the default. The lookup only reads the
				// cache, so a cold catalog never delays a request.
				if reason == "default" {
					if id, ok := p.catalog.Lookup(m); ok {
						target, reason = id, "catalog"
					} else if route := cfg.Models.RouteNamed(m); route != nil && route.ID != "" {
						// Models outside the subscription are declared by their
						// route, so they never reach the registry.
						target, reason = strings.ToLower(route.ID), "route"
					}
				}
				// Auxiliary calls (background summarising, telemetry-ish
				// one-shots) do not need the main model. Compaction is
				// deliberately excluded: it rewrites the context and a cheap
				// model would degrade every later turn.
				if strings.EqualFold(class, "auxiliary") && r.Header.Get("X-Claude-Code-Compaction") == "" &&
					cfg.Models.Background != "" && !strings.EqualFold(target, cfg.Models.Background) {
					target, reason = strings.ToLower(cfg.Models.Background), "background"
				}
				data["model"] = target
				if rewritten, err := json.Marshal(data); err == nil {
					body = rewritten
					modelIn, modelOut, why = m, target, reason
				}
			}
		}
	}

	ctx := r.Context()

	// Each agent chain gets its own upstream cell, so parallel subagents stop
	// colliding on one cell and refusing each other by prefill. Token counting
	// is the exception: it prefills nothing, and the app asks for it constantly,
	// so a cell of its own would only queue real answers behind telemetry.
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens" {
		select {
		case p.countSlots <- struct{}{}:
			defer func() { <-p.countSlots }()
		case <-ctx.Done():
			aborted = true
			return
		}
	} else {
		release, wait, aerr := p.sched.Acquire(ctx, cell,
			prio, time.Duration(cfg.Scheduler.QueueTimeoutMS)*time.Millisecond)
		waited = wait
		if aerr != nil {
			status = http.StatusTooManyRequests
			if errors.Is(aerr, scheduler.ErrQueueTimeout) {
				status = http.StatusTooManyRequests
				w.Header().Set("Retry-After", "5")
				writeOverloaded(w, "no upstream cell available within queue_timeout_ms")
			} else if ctx.Err() == nil {
				http.Error(w, aerr.Error(), http.StatusBadGateway)
				status = http.StatusBadGateway
			} else {
				aborted = true
			}
			return
		}
		defer release()
		p.sched.ObserveQueueWait(waited)
	}

	base, err := p.upstreamRequest(ctx, r, cfg, body, cell, cfg.Models.RouteFor(modelOut))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		status = http.StatusBadGateway
		return
	}
	capture = newCaptureRecord(cfg, base.Header, body)

	// Streaming requests own their retry loop, because a client that sees no
	// bytes assumes the stream died. serveStream keeps it fed with pings.
	if streamReq {
		// Headers must be in place before the keepalive writes the status
		// line, and before the first upstream event is relayed.
		p.applyQuota(w.Header(), cfg)
		status, attempts, acc, aborted = p.serveStream(ctx, w, cfg, base, body, modelIn, capture, cell)
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens" {
		billable = false
		res, err := p.countTokens(ctx, cfg, base, body, cell)
		if err != nil {
			if aborted = ctx.Err() != nil; !aborted {
				http.Error(w, err.Error(), http.StatusBadGateway)
				status = http.StatusBadGateway
			}
			return
		}
		status, attempts = res.status, res.attempts
		if capture != nil && cfg.Log.Capture.ResponseHeaders {
			capture.ResponseHeaders = reqlog.MaskHeaders(res.header)
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
	}, cell)
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
		capture.ResponseHeaders = reqlog.MaskHeaders(resp.Header)
	}
	if status >= 200 && status < 300 {
		p.applyQuota(w.Header(), cfg)
	}
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	acc, aborted = relayResponse(ctx, w, resp, capture, cfg, modelIn)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (p *Proxy) upstreamRequest(ctx context.Context, r *http.Request, cfg *Config, body []byte, session string, route *ModelRoute) (*http.Request, error) {
	upstream := cfg.Upstream
	if route != nil && route.BaseURL != "" {
		upstream = route.BaseURL
	}
	url := strings.TrimRight(upstream, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, values := range r.Header {
		if !forwardHeader(strings.ToLower(name)) {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("x-opencode-session", session)
	if route != nil {
		// The route's own credential replaces whatever the client sent: the
		// client holds the subscription token, and this endpoint wants a
		// different one.
		token := route.Token()
		if token == "" && (route.TokenEnv != "" || route.TokenFile != "") {
			return nil, fmt.Errorf("route %s: no credential in %s", route.ID,
				firstNonEmpty(route.TokenEnv, route.TokenFile))
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Del("x-api-key")
		}
	}
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
		// A synthesized quota header beats an upstream duplicate: the two
		// would otherwise reach the client as two values and confuse the
		// usage meter.
		if dst.Get(name) != "" && strings.HasPrefix(strings.ToLower(name), "anthropic-ratelimit-unified-") {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// doAttempts forwards one request, retrying 429 and 5xx responses until the
// attempt count or the total wait budget is exhausted. It returns the final
// response (whose headers have not been written to the client yet) and the
// number of attempts made.
func (p *Proxy) doAttempts(ctx context.Context, cfg *Config, newReq func() (*http.Request, error), cell string) (*http.Response, int, error) {
	policy := retry.NewRetryPolicy(cfg.Retry, nil)
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
			// A prefill refusal is the gateway saying too many cells are being
			// prefilled at once; every other 429 (allowance, throttling) says
			// nothing about concurrency and must not move the ceiling.
			if prefillRefusal(peekResponseBody(resp)) {
				p.prefill429.Add(1)
				p.sched.NotePrefillRefusal()
			}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.sched.ReportSuccess(cell)
		} else if retry.ShouldRetryResponse(resp) {
			p.sched.ReportFailure(cell)
		}
		if !retry.ShouldRetryResponse(resp) || attempt >= maxAttempts {
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

// prefillRefusal reports whether an upstream body is a prefill capacity
// refusal rather than any other kind of 429.
func prefillRefusal(body []byte) bool {
	return bytes.Contains(bytes.ToLower(body), []byte("prefill"))
}

// peekResponseBody reads a bounded copy of the body and puts it back, so the
// response can still be forwarded byte for byte afterwards.
func peekResponseBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	return body
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
func relayResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *reqlog.CaptureRecord, cfg *Config, modelIn string) (usageAccumulator, bool) {
	var (
		parser   *sseParser
		raw      bytes.Buffer
		captured *limitedBuffer
		acc      usageAccumulator
	)

	// A JSON (non-streaming) response is small enough to rewrite before it is
	// sent, so the client sees the model id it asked for.
	if !isEventStream(resp.Header) && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		body, err := io.ReadAll(resp.Body)
		if rec != nil && cfg.Log.Capture.ResponseBody {
			text, truncated := truncateBody(body, cfg.Log.MaxBodyBytes)
			rec.ResponseBody = text
			if truncated {
				rec.Truncated = true
			}
		}
		body = rewriteModelJSON(body, modelIn)
		body = thinking.AddSummariesJSON(body)
		aborted := false
		if _, werr := w.Write(body); werr != nil {
			aborted = true
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if err != nil && ctx.Err() != nil {
			aborted = true
		}
		return parseUsageJSON(body), aborted
	}

	if isEventStream(resp.Header) {
		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
		parser = &sseParser{
			out:         w,
			flusher:     flusher,
			modelIn:     modelIn,
			summaryWait: time.Duration(cfg.Scheduler.ThinkingSummaryWaitMS) * time.Millisecond,
		}
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
			if captured != nil {
				captured.Write(chunk)
			}
			if parser != nil {
				if !parser.Feed(chunk) {
					aborted = true
					break
				}
			} else {
				raw.Write(chunk)
				if _, werr := w.Write(chunk); werr != nil {
					aborted = true
					break
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
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
		if !parser.Close() {
			aborted = true
		}
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

// rewriteModelJSON replaces the response's model id with the one the client
// asked for, so a picker entry is echoed back instead of the upstream id.
func rewriteModelJSON(body []byte, modelIn string) []byte {
	if modelIn == "" || len(body) == 0 || !json.Valid(body) {
		return body
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		return body
	}
	if _, ok := data["model"].(string); !ok {
		return body
	}
	data["model"] = modelIn
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

// rewriteSSEModel rewrites the model inside a message_start event line.
func rewriteSSEModel(line []byte, modelIn string) []byte {
	if modelIn == "" || !bytes.Contains(line, []byte(`"message_start"`)) {
		return line
	}
	payload := bytes.TrimPrefix(line, []byte("data:"))
	payload = bytes.TrimSpace(payload)
	if !json.Valid(payload) {
		return line
	}
	var ev map[string]any
	if json.Unmarshal(payload, &ev) != nil {
		return line
	}
	msg, ok := ev["message"].(map[string]any)
	if !ok {
		return line
	}
	msg["model"] = modelIn
	out, err := json.Marshal(ev)
	if err != nil {
		return line
	}
	res := make([]byte, 0, len(out)+6)
	res = append(res, "data: "...)
	res = append(res, out...)
	return res
}

func isEventStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// sseParser reconstructs SSE lines from arbitrary read chunks: bytes after
// the last newline stay pending until the next chunk completes the line.
type sseParser struct {
	// mu serialises the copy loop with the hold's release timer.
	mu      sync.Mutex
	pending []byte
	acc     usageAccumulator
	out     io.Writer
	flusher http.Flusher
	modelIn string
	// summaryWait bounds how long a thinking block's start event is held back
	// while a one-line summary is collected. 0 disables the hold.
	summaryWait time.Duration
	hold        *thinking.Hold
}

func (s *sseParser) Feed(chunk []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil && time.Since(s.hold.Started) > s.hold.Wait {
		if !s.flushHold() {
			return false
		}
	}
	s.pending = append(s.pending, chunk...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			return true
		}
		line := s.pending[:i]
		if !s.emit(line) {
			return false
		}
		s.pending = s.pending[i+1:]
	}
}

// emit forwards one SSE line. A thinking block's start event is held back
// until a summary line is collected or the wait budget runs out; the deltas
// buffered with it are replayed in order when it ships.
func (s *sseParser) emit(line []byte) bool {
	if s.hold != nil {
		if s.hold.IsDelta(line) {
			s.hold.Deltas = append(s.hold.Deltas, append([]byte(nil), line...))
			s.hold.Text.WriteString(thinking.DeltaText(line))
			return s.settleHold()
		}
		// Event lines, blank separators and the block's own signature travel
		// with the held start; anything else means the block is over.
		if thinking.IsRelated(line, s.hold.Index) {
			s.hold.Deltas = append(s.hold.Deltas, append([]byte(nil), line...))
			return true
		}
		if !s.flushHold() {
			return false
		}
	}
	if h := thinking.NewHold(line, s.summaryWait); h != nil {
		s.hold = h
		h.Timer = time.AfterFunc(h.Wait, func() { s.expireHold(h) })
		s.processLine(line)
		return true
	}
	s.processLine(line)
	if s.out != nil {
		if _, err := s.out.Write(rewriteSSEModel(line, s.modelIn)); err != nil {
			return false
		}
		if _, err := s.out.Write([]byte("\n")); err != nil {
			return false
		}
		if s.flusher != nil {
			s.flusher.Flush()
		}
	}
	return true
}

// settleHold ships the held block once a readable first line is complete, or
// gives it the generic one once the byte cap or the wait budget is spent.
func (s *sseParser) settleHold() bool {
	h := s.hold
	text := h.Text.String()
	sum := thinking.FirstMeaningfulLine(text)
	complete := sum != "" && (strings.Contains(text, "\n") ||
		(len([]rune(sum)) >= 24 && strings.ContainsAny(sum, ".!?")))
	if complete {
		return s.flushHoldWith(thinking.Truncate(sum, thinking.MaxChars))
	}
	if h.Text.Len() >= thinking.MaxBytes || time.Since(h.Started) > h.Wait {
		return s.flushHold()
	}
	return true
}

// expireHold releases a block whose model paused for the whole wait budget.
func (s *sseParser) expireHold(h *thinking.Hold) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != h {
		return
	}
	sum := thinking.FirstMeaningfulLine(h.Text.String())
	if sum == "" {
		sum = thinking.FallbackSummary
	}
	s.flushHoldWith(thinking.Truncate(sum, thinking.MaxChars))
}

func (s *sseParser) flushHold() bool {
	h := s.hold
	sum := thinking.FirstMeaningfulLine(h.Text.String())
	if sum == "" {
		sum = thinking.FallbackSummary
	}
	return s.flushHoldWith(thinking.Truncate(sum, thinking.MaxChars))
}

func (s *sseParser) flushHoldWith(summary string) bool {
	h := s.hold
	s.hold = nil
	if h.Timer != nil {
		h.Timer.Stop()
	}
	if s.out == nil {
		return true
	}
	if _, err := s.out.Write(thinking.WithSummary(h.Line, summary)); err != nil {
		return false
	}
	if _, err := s.out.Write([]byte("\n")); err != nil {
		return false
	}
	for _, d := range h.Deltas {
		if _, err := s.out.Write(d); err != nil {
			return false
		}
		if _, err := s.out.Write([]byte("\n")); err != nil {
			return false
		}
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

func (s *sseParser) Close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil && !s.flushHold() {
		return false
	}
	if len(s.pending) > 0 {
		ok := s.emit(s.pending)
		s.pending = nil
		return ok
	}
	return true
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

func newCaptureRecord(cfg *Config, header http.Header, body []byte) *reqlog.CaptureRecord {
	cc := cfg.Log.Capture
	if !cc.RequestHeaders && !cc.RequestBody && !cc.ResponseHeaders && !cc.ResponseBody {
		return nil
	}
	rec := &reqlog.CaptureRecord{}
	if cc.RequestHeaders {
		rec.RequestHeaders = reqlog.MaskHeaders(header)
	}
	if cc.RequestBody {
		rec.RequestBody, rec.Truncated = truncateBody(body, cfg.Log.MaxBodyBytes)
	}
	return rec
}

func fillCapturedBody(rec *reqlog.CaptureRecord, cfg *Config, body []byte) {
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

func (p *Proxy) countTokens(ctx context.Context, cfg *Config, base *http.Request, body []byte, cell string) (*cachedResponse, error) {
	return p.tokens.getOrLoad(ctx, tokenCacheKey(body), func() (*cachedResponse, error) {
		resp, attempts, err := p.doAttempts(ctx, cfg, func() (*http.Request, error) {
			return cloneRequestBody(ctx, base, body), nil
		}, cell)
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
