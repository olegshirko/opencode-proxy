package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// failAfterWriter simulates a client socket that dies mid-stream: every write
// after the threshold returns an error, so the relay loop aborts like it does
// for a walked-away user.
type failAfterWriter struct {
	httptest.ResponseRecorder
	failAfter int
	written   int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written >= w.failAfter {
		return 0, errors.New("client gone")
	}
	n, err := w.ResponseRecorder.Write(p)
	w.written += n
	return n, err
}

func (w *failAfterWriter) Flush() {}

// TestQoderStreamAbandonNoLeak: when the relay loop aborts mid-stream (client
// socket died), the translating reader's goroutine must unwind instead of
// staying blocked on the pipe forever.
func TestQoderStreamAbandonNoLeak(t *testing.T) {
	h, fake := newQoderHarness(t, "pat-secret")
	fake.slowStream.Store(true)

	fw := &failAfterWriter{ResponseRecorder: *httptest.NewRecorder(), failAfter: 512}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-qoder-test","max_tokens":64,"stream":true,
			"messages":[{"role":"user","content":"Hi"}]}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.proxy.ServeHTTP(fw, req)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never returned after simulated client death")
	}

	// The handler has returned (upstream body closed via defer); the reader
	// goroutine must not stay blocked on the pipe.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !stackHas("qoderchat.TranslateStream") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("leak: TranslateStream goroutine still blocked after relay aborted")
}
