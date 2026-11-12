package proxy

import (
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// trackingSrc produces a long SSE stream slowly and records Close. It is the
// upstream body a translating reader wraps. Close is called concurrently by
// the client and the translator goroutine, so the flag must be atomic.
type trackingSrc struct {
	chunks []string
	i      int
	closed atomic.Bool
}

func (s *trackingSrc) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		return 0, io.EOF
	}
	n := copy(p, s.chunks[s.i])
	s.i++
	return n, nil
}

func (s *trackingSrc) Close() error {
	s.closed.Store(true)
	return nil
}

func stackHas(substr string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), substr)
}

func TestTranslateReaderCloseUnwinds(t *testing.T) {
	var chunks []string
	for i := 0; i < 500; i++ { // far more than a client would ever read
		chunks = append(chunks, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	src := &trackingSrc{chunks: chunks}
	r := translateReader(src, "m")
	defer r.Close() // idempotent; the explicit Close below does the asserting

	buf := make([]byte, 32)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	r.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if src.closed.Load() && !stackHas("qoderchat.TranslateStream") {
			return // fully unwound
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("leak: src.closed=%v, TranslateStream goroutine alive=%v", src.closed.Load(), stackHas("qoderchat.TranslateStream"))
}
