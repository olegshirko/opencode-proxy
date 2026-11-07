package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamThinkingSummaryInjected(t *testing.T) {
	sse := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me consider the options here"}}` + "\n\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	startIdx := strings.Index(body, "content_block_start")
	deltaIdx := strings.Index(body, "thinking_delta")
	if startIdx < 0 || deltaIdx < 0 {
		t.Fatalf("missing events:\n%s", body)
	}
	if startIdx > deltaIdx {
		t.Errorf("content_block_start arrived after its delta:\n%s", body)
	}
	if !strings.Contains(body, `"summary":"Let me consider the options here"`) {
		t.Errorf("no summary injected into the thinking block:\n%s", body)
	}
	if !strings.Contains(body, `"type":"summary_text"`) {
		t.Errorf("summary entry has the wrong type:\n%s", body)
	}
	// The raw thinking text must survive untouched.
	if !strings.Contains(body, `"thinking":"Let me consider the options here"`) {
		t.Errorf("raw thinking text was altered:\n%s", body)
	}
}

func TestStreamThinkingFallbackAfterWait(t *testing.T) {
	sse := `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"   "}}` + "\n\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}, func(c *Config) {
		c.Scheduler.ThinkingSummaryWaitMS = 10
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, `"summary":"Thinking"`) {
		t.Errorf("blank thinking did not fall back to the generic summary:\n%s", body)
	}
	if !strings.Contains(body, "content_block_stop") {
		t.Errorf("stop event lost:\n%s", body)
	}
}

func TestNonStreamThinkingSummaries(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","model":"glm-5.3","content":[` +
			`{"type":"thinking","thinking":"## First idea\n- second line","signature":"sig"},` +
			`{"type":"text","text":"answer"}]}`))
	})

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, `"summary":"First idea"`) {
		t.Errorf("markdown heading was not stripped from the summary:\n%s", body)
	}
	if !strings.Contains(body, `"signature":"sig"`) {
		t.Errorf("signature was altered:\n%s", body)
	}
}

func TestSummariesStrippedFromHistory(t *testing.T) {
	got := make(chan string, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1<<16)
		n, _ := r.Body.Read(body)
		got <- string(body[:n])
		w.WriteHeader(http.StatusOK)
	})

	h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"raw","signature":"s","summaries":[{"type":"summary_text","summary":"raw"}]}]}]}`,
		nil)

	upstream := <-got
	if strings.Contains(upstream, "summaries") {
		t.Errorf("summaries leaked upstream:\n%s", upstream)
	}
	if !strings.Contains(upstream, `"thinking":"raw"`) {
		t.Errorf("thinking text itself must stay in the history:\n%s", upstream)
	}
}

func TestCleanMarkdownLine(t *testing.T) {
	cases := map[string]string{
		"## Heading":        "Heading",
		"- bullet point":    "bullet point",
		"1. numbered":       "numbered",
		"`code` note":       "code note",
		"[link](http://x)":  "link",
		"  spaced  ":        "spaced",
		"":                  "",
		"###":               "",
		"plain text":        "plain text",
		"see [docs](/d) now": "see docs now",
	}
	for in, want := range cases {
		if got := cleanMarkdownLine(in); got != want {
			t.Errorf("cleanMarkdownLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstMeaningfulLineSkipsDecorations(t *testing.T) {
	text := "```\ncode fence\n```\n\n**Real thought**"
	if got := firstMeaningfulLine(text); got != "code fence" {
		t.Errorf("firstMeaningfulLine = %q, want the first readable line", got)
	}
}

func TestSummaryTruncation(t *testing.T) {
	long := strings.Repeat("а", 300)
	got := truncateRunes(long, 160)
	if len([]rune(got)) != 161 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncateRunes produced %d runes, want 160 plus the ellipsis", len([]rune(got)))
	}
}

func TestHoldReleasesBeforeUnrelatedEvent(t *testing.T) {
	// A stop event for the held block must flush it first: the client must see
	// start, then stop, never stop before start.
	sse := `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}, func(c *Config) {
		c.Scheduler.ThinkingSummaryWaitMS = 500
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	startIdx := strings.Index(body, "content_block_start")
	stopIdx := strings.Index(body, "content_block_stop")
	if startIdx > stopIdx {
		t.Errorf("start was reordered after stop:\n%s", body)
	}
	if !strings.Contains(body, `"summary":"Thinking"`) {
		t.Errorf("no summary at all:\n%s", body)
	}
}

func TestStreamThinkingSummarySurvivesEventLines(t *testing.T) {
	// The gateway puts an "event:" line before every data line; those lines
	// belong to the held block and must not end the hold before its text has
	// been seen, which used to leave every summary at the generic fallback.
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m","role":"assistant"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"We need answer user."}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}, func(c *Config) {
		c.Scheduler.ThinkingSummaryWaitMS = 300
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, `"summary":"We need answer user."`) {
		t.Errorf("summary was not taken from the thinking text:\n%s", body)
	}
	if strings.Contains(body, `"summary":"Thinking"`) {
		t.Errorf("summary fell back to the generic one:\n%s", body)
	}
}

func TestStreamThinkingSummaryWaitDisabled(t *testing.T) {
	sse := `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}, func(c *Config) {
		c.Scheduler.ThinkingSummaryWaitMS = 0
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "content_block_start") || strings.Contains(body, "summaries") {
		t.Errorf("hold should be disabled with wait=0:\n%s", body)
	}
}

func TestExpireHoldShipsAStartWithoutFurtherData(t *testing.T) {
	var out bytes.Buffer
	p := &sseParser{out: &out, summaryWait: time.Hour}
	line := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	h := maybeHoldThinkingStart(line, time.Hour)
	if h == nil {
		t.Fatal("a thinking start was not held")
	}
	p.hold = h
	h.text.WriteString("Collecting the relevant files")

	p.expireHold(h)

	if p.hold != nil {
		t.Fatal("the hold survived its timer")
	}
	if !strings.Contains(out.String(), `"summary":"Collecting the relevant files"`) {
		t.Fatalf("the timer did not ship the collected summary:\n%s", out.String())
	}
}

func TestCloseEmitsAPendingPartialLine(t *testing.T) {
	var out bytes.Buffer
	p := &sseParser{out: &out}
	p.pending = []byte(`data: {"type":"ping"}`)
	if !p.Close() {
		t.Fatal("Close reported a write failure")
	}
	if !strings.Contains(out.String(), `"type":"ping"`) {
		t.Fatalf("the trailing line was dropped:\n%s", out.String())
	}
	if p.pending != nil {
		t.Fatal("pending bytes were not cleared")
	}
}
