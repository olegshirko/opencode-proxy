package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode-proxy/internal/config"
	"opencode-proxy/internal/models"
	"opencode-proxy/internal/qoderauth"
	"opencode-proxy/internal/qoderchat"
	"opencode-proxy/internal/scheduler"
)

// qoderUUID is the X-Request-ID / X-Session-ID value Qoder's API expects.
func qoderUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return newRequestID() // never expected; fall back to the counter id
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// handleQoder serves a request whose resolved route speaks Qoder's chat API.
// count_tokens never reaches the network (there is no upstream equivalent):
// it answers with the local estimate. Everything else is translated,
// dispatched with a jobToken, and translated back; stream:false clients get
// the upstream SSE assembled into one message body (streaming has its own
// path, wired in handleQoderStream).
func (p *Proxy) handleQoder(w http.ResponseWriter, r *http.Request, cfg *config.Config,
	body []byte, route *models.Route, modelIn, modelOut string, cell string, prio int, streamReq bool,
) (status, attempts int, acc usageAccumulator, aborted, billable bool) {
	ctx := r.Context()
	billable = true

	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens" {
		billable = false
		n, err := qoderchat.EstimateTokens(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			status = http.StatusBadRequest
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"input_tokens":%d}`, n)
		status = http.StatusOK
		return
	}

	meta := qoderchat.RequestMeta{
		RequestID:  qoderUUID(),
		SessionID:  qoderUUID(),
		ClientType: "opencode-proxy",
	}
	translated, err := qoderchat.TranslateRequest(body, modelOut, meta)
	if err != nil {
		status = http.StatusBadRequest
		billable = false
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	release, _, aerr := p.sched.Acquire(ctx, cell, prio,
		time.Duration(cfg.Scheduler.QueueTimeoutMS)*time.Millisecond)
	if aerr != nil {
		status = http.StatusTooManyRequests
		billable = false
		if errors.Is(aerr, scheduler.ErrQueueTimeout) {
			w.Header().Set("Retry-After", "5")
			writeOverloaded(w, "qoder: no upstream cell available within queue_timeout_ms")
		} else if ctx.Err() == nil {
			http.Error(w, aerr.Error(), http.StatusBadGateway)
		} else {
			aborted = true
		}
		return
	}
	defer release()

	base := route.BaseURL
	if base == "" {
		base = cfg.Qoder.InferenceBaseURL
	}

	resp, tries, err := p.qoderAttempt(ctx, p.qoderManagerFor(cfg), base, translated, meta, modelOut, streamReq)
	if err != nil {
		if ctx.Err() != nil {
			aborted = true
			return
		}
		status = http.StatusBadGateway
		billable = false
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	attempts = tries
	defer resp.Body.Close()

	status = resp.StatusCode
	if status < 200 || status >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		http.Error(w, strings.TrimSpace(string(payload)), status)
		return
	}

	if streamReq {
		// The upstream SSE is OpenAI-shaped; hand the client a response whose
		// body is the same stream translated to Anthropic events. relayResponse
		// then does what it does for OpenCode: parse, think-plates, usage acc.
		synthetic := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       translateReader(resp.Body, modelOut),
		}
		acc, aborted = relayResponse(ctx, w, synthetic, nil, cfg, modelIn)
		return
	}

	out, usage, err := qoderchat.CollectNonStream(resp.Body, modelOut)
	if err != nil {
		if ctx.Err() != nil {
			aborted = true
			return
		}
		status = http.StatusBadGateway
		http.Error(w, fmt.Sprintf("qoder: bad response: %v", err), http.StatusBadGateway)
		return
	}
	acc.in = float64(usage.InputTokens)
	acc.out = float64(usage.OutputTokens)
	acc.cache = float64(usage.CacheReadTokens)
	acc.haveIn, acc.haveCache = true, usage.CacheReadTokens > 0
	// Cost and allowance stay zero: Qoder is a separate subscription and does
	// not consume the OpenCode budget windows (design spec, "Цены и учёт").

	if out == nil {
		status = http.StatusBadGateway
		http.Error(w, "qoder: empty response", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return
}

// qoderManagerFor returns the token manager matching the current qoder
// config section, rebuilding it when hot reload changed the section. An
// in-flight cached token dies with the old manager, which is fine: the next
// request exchanges again.
func (p *Proxy) qoderManagerFor(cfg *config.Config) *qoderauth.Manager {
	p.qoderMu.Lock()
	defer p.qoderMu.Unlock()
	if p.qoderAuth == nil || p.qoderCfg != cfg.Qoder {
		p.qoderCfg = cfg.Qoder
		p.qoderAuth = qoderauth.New(cfg.Qoder, p.client, nil)
	}
	return p.qoderAuth
}

// qoderAttempt dispatches one translated request, re-authenticating once when
// the jobToken was rejected mid-flight.
func (p *Proxy) qoderAttempt(ctx context.Context, mgr *qoderauth.Manager, baseURL string,
	translated []byte, meta qoderchat.RequestMeta, modelOut string, stream bool,
) (*http.Response, int, error) {
	tok, err := mgr.Token(ctx)
	if err != nil {
		return nil, 0, err
	}
	for try := 1; ; try++ {
		req, rerr := buildQoderUpstream(ctx, baseURL, translated, meta, tok)
		if rerr != nil {
			return nil, try, rerr
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, try, fmt.Errorf("qoder: upstream: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 1 {
			// The jobToken went stale mid-flight: drop it, re-exchange, retry
			// once. Anything else passes through untouched.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			mgr.Invalidate()
			if tok, err = mgr.Token(ctx); err != nil {
				return nil, try, err
			}
			continue
		}
		return resp, try, nil
	}
}

// translateReader converts an OpenAI-shaped SSE stream into Anthropic events
// on the fly: the relay pipeline downstream never sees the difference.
func translateReader(src io.ReadCloser, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		_, err := qoderchat.TranslateStream(src, pw, model)
		_ = src.Close()
		_ = pw.CloseWithError(err) // nil err closes cleanly
	}()
	return pr
}

func buildQoderUpstream(ctx context.Context, baseURL string, translatedBody []byte,
	meta qoderchat.RequestMeta, token string,
) (*http.Request, error) {
	url := strings.TrimRight(baseURL, "/") + "/model/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translatedBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-ID", meta.RequestID)
	req.Header.Set("X-Session-ID", meta.SessionID)
	return req, nil
}
