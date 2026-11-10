package qoderauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpenAPI stands in for openapi.qoder.sh: it counts exchange and refresh
// calls and returns canned jobToken JSON.
type fakeOpenAPI struct {
	mu        sync.Mutex
	exchanges int
	refreshes int
	exchange  string // raw JSON body for POST /api/v1/jobToken/exchange
	refresh   string // raw JSON body for POST /api/v1/jobToken/refresh
	refreshPb []byte // last refresh request body
	exPb      []byte // last exchange request body
}

func (f *fakeOpenAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := make([]byte, r.ContentLength)
	_, _ = r.Body.Read(body)
	switch r.URL.Path {
	case "/api/v1/jobToken/exchange":
		f.exchanges++
		f.exPb = body
		w.Write([]byte(f.exchange))
	case "/api/v1/jobToken/refresh":
		f.refreshes++
		f.refreshPb = body
		w.Write([]byte(f.refresh))
	default:
		http.NotFound(w, r)
	}
}

func tokenResp(token, refresh string, expiresIn int64) string {
	return fmt.Sprintf(`{"token":%q,"refresh_token":%q,"expires_in":%d,"refresh_token_expires_in":86400}`,
		token, refresh, expiresIn)
}

func newManager(t *testing.T, cfg Config, srv *httptest.Server, now func() time.Time) *Manager {
	t.Helper()
	cfg.OpenAPIBaseURL = srv.URL
	m := New(cfg, srv.Client(), now)
	return m
}

func TestExchangeThenCached(t *testing.T) {
	f := &fakeOpenAPI{exchange: tokenResp("tok1", "ref1", 3600)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("QODER_TEST_PAT", "pat-secret")
	m := newManager(t, Config{TokenEnv: "QODER_TEST_PAT", RefreshMarginMS: 60000}, srv, time.Now)

	ctx := context.Background()
	tok, err := m.Token(ctx)
	if err != nil || tok != "tok1" {
		t.Fatalf("first token: %q err=%v", tok, err)
	}
	tok2, err := m.Token(ctx)
	if err != nil || tok2 != "tok1" {
		t.Fatalf("second token: %q err=%v", tok2, err)
	}
	if f.exchanges != 1 {
		t.Fatalf("expected 1 exchange, got %d", f.exchanges)
	}
}

func TestRefreshBeforeExpiry(t *testing.T) {
	f := &fakeOpenAPI{
		exchange: tokenResp("tok1", "ref1", 3600),
		refresh:  tokenResp("tok2", "ref2", 3600),
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("QODER_TEST_PAT", "pat-secret")

	now := time.Unix(1_700_000_000, 0)
	m := newManager(t, Config{TokenEnv: "QODER_TEST_PAT", RefreshMarginMS: 60_000}, srv, func() time.Time { return now })

	ctx := context.Background()
	if _, err := m.Token(ctx); err != nil {
		t.Fatalf("first token: %v", err)
	}
	if !strings.Contains(string(f.exPb), `"personal_token":"pat-secret"`) {
		t.Fatalf("exchange body missing personal_token: %s", f.exPb)
	}
	// 3550s in: the 3600s token has 50s left — inside the 60s refresh margin.
	now = now.Add(3550 * time.Second)
	tok, err := m.Token(ctx)
	if err != nil || tok != "tok2" {
		t.Fatalf("refreshed token: %q err=%v", tok, err)
	}
	if f.exchanges != 1 || f.refreshes != 1 {
		t.Fatalf("exchanges=%d refreshes=%d, want 1/1", f.exchanges, f.refreshes)
	}
	if !strings.Contains(string(f.refreshPb), `"refresh_token":"ref1"`) {
		t.Fatalf("refresh body missing refresh_token: %s", f.refreshPb)
	}
}

func TestInvalidateForcesReExchange(t *testing.T) {
	f := &fakeOpenAPI{exchange: tokenResp("tok1", "ref1", 3600)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("QODER_TEST_PAT", "pat-secret")
	m := newManager(t, Config{TokenEnv: "QODER_TEST_PAT"}, srv, time.Now)

	ctx := context.Background()
	if _, err := m.Token(ctx); err != nil {
		t.Fatalf("first token: %v", err)
	}
	m.Invalidate()
	tok, err := m.Token(ctx)
	if err != nil || tok != "tok1" {
		t.Fatalf("token after invalidate: %q err=%v", tok, err)
	}
	if f.exchanges != 2 || f.refreshes != 0 {
		t.Fatalf("exchanges=%d refreshes=%d, want 2/0", f.exchanges, f.refreshes)
	}
}

func TestMissingPATFails(t *testing.T) {
	f := &fakeOpenAPI{exchange: tokenResp("tok1", "ref1", 3600)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := newManager(t, Config{}, srv, time.Now) // no token_env, no token_file

	_, err := m.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("want error naming token_file/token_env, got %v", err)
	}
	if f.exchanges != 0 {
		t.Fatalf("exchange attempted without PAT: %d", f.exchanges)
	}
}

func TestStatusNeverContainsToken(t *testing.T) {
	f := &fakeOpenAPI{exchange: tokenResp("tok1-secret-value", "ref1-secret", 3600)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("QODER_TEST_PAT", "pat-secret-value")
	m := newManager(t, Config{TokenEnv: "QODER_TEST_PAT"}, srv, time.Now)

	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	raw, err := json.Marshal(m.Status())
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "tok1-secret-value") || strings.Contains(s, "ref1-secret") || strings.Contains(s, "pat-secret-value") {
		t.Fatalf("status leaks secrets: %s", s)
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.State != "ready" || !st.HasRefresh || st.Exchanges != 1 {
		t.Fatalf("status: %+v", st)
	}
}

func TestTokenFieldOrder(t *testing.T) {
	// token wins over device_token; device_token wins over access_token.
	f := &fakeOpenAPI{exchange: `{"device_token":"dtok","access_token":"atok","refresh_token":"ref1","expires_in":3600}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("QODER_TEST_PAT", "pat-secret")
	m := newManager(t, Config{TokenEnv: "QODER_TEST_PAT"}, srv, time.Now)

	tok, err := m.Token(context.Background())
	if err != nil || tok != "dtok" {
		t.Fatalf("want device_token picked, got %q err=%v", tok, err)
	}

	f2 := &fakeOpenAPI{exchange: `{"access_token":"atok","refresh_token":"ref1","expires_in":3600}`}
	srv2 := httptest.NewServer(f2)
	defer srv2.Close()
	m2 := newManager(t, Config{TokenEnv: "QODER_TEST_PAT"}, srv2, time.Now)
	tok2, err := m2.Token(context.Background())
	if err != nil || tok2 != "atok" {
		t.Fatalf("want access_token picked, got %q err=%v", tok2, err)
	}
}

func TestTokenFilePAT(t *testing.T) {
	f := &fakeOpenAPI{exchange: tokenResp("tok1", "ref1", 3600)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	pat := dir + "/pat.txt"
	if err := os.WriteFile(pat, []byte("  file-pat  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, Config{TokenFile: pat}, srv, time.Now)
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	if !strings.Contains(string(f.exPb), `"personal_token":"file-pat"`) {
		t.Fatalf("file PAT not trimmed: %s", f.exPb)
	}
}
