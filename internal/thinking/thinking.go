package thinking

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxChars caps the summary the desktop app shows on the collapsed
	// thinking plate.
	MaxChars = 160
	// MaxBytes stops text accumulation: past this there is still no complete
	// meaningful line, so the block ships with a generic summary rather than
	// growing the buffer.
	MaxBytes = 256
	// FallbackSummary is used when no meaningful line was collected within
	// the wait window.
	FallbackSummary = "Thinking"
)

var markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
var listMarker = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)

// FirstMeaningfulLine returns the first readable line of a thinking text,
// stripped of markdown decoration, or "" when there is none yet.
func FirstMeaningfulLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = cleanMarkdownLine(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// cleanMarkdownLine strips headings, list markers, backticks and link URLs,
// keeping the link text.
func cleanMarkdownLine(line string) string {
	s := strings.TrimSpace(line)
	s = strings.TrimLeft(s, "#")
	s = strings.TrimSpace(s)
	s = listMarker.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "`", "")
	s = markdownLink.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

func Truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// StripSummaries removes summary injections from the conversation
// history: they exist for the desktop UI and must not leak back upstream.
func StripSummaries(data map[string]any) {
	messages, ok := data["messages"].([]any)
	if !ok {
		return
	}
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "thinking" {
				continue
			}
			delete(b, "summaries")
		}
	}
}

// AddSummariesJSON injects a one-line summary into every thinking
// block of a non-streaming response, so the app renders it collapsed.
func AddSummariesJSON(body []byte) []byte {
	if len(body) == 0 || !json.Valid(body) {
		return body
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		return body
	}
	content, ok := data["content"].([]any)
	if !ok {
		return body
	}
	changed := false
	for _, b := range content {
		blk, ok := b.(map[string]any)
		if !ok || blk["type"] != "thinking" {
			continue
		}
		if _, has := blk["summaries"]; has {
			continue
		}
		text, _ := blk["thinking"].(string)
		sum := FirstMeaningfulLine(text)
		if sum == "" {
			sum = FallbackSummary
		}
		blk["summaries"] = []map[string]string{{"type": "summary_text", "summary": Truncate(sum, MaxChars)}}
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

// Hold is the state of a thinking block whose content_block_start is
// being held back while its summary is collected. Only the start event is
// delayed; deltas and stop keep their order, they are buffered with it.
type Hold struct {
	Wait    time.Duration
	Started time.Time
	Line    []byte
	Index   int
	Deltas  [][]byte
	Text    bytes.Buffer
	// Timer releases the hold when the model pauses between thinking bursts;
	// without it a long gap would keep the block from reaching the client.
	Timer *time.Timer
}

func NewHold(line []byte, wait time.Duration) *Hold {
	if wait <= 0 {
		return nil
	}
	payload, ok := sseDataPayload(line)
	if !ok {
		return nil
	}
	var ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type      string          `json:"type"`
			Summaries json.RawMessage `json:"summaries"`
		} `json:"content_block"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return nil
	}
	if ev.Type != "content_block_start" || ev.ContentBlock.Type != "thinking" || len(ev.ContentBlock.Summaries) > 0 {
		return nil
	}
	return &Hold{
		Wait:    wait,
		Started: time.Now(),
		Line:    append([]byte(nil), line...),
		Index:   ev.Index,
	}
}

// IsDeltaFor reports whether the line is a content_block_delta of the given
// delta type for the held block index.
func IsDeltaFor(line []byte, index int, kind string) bool {
	payload, ok := sseDataPayload(line)
	if !ok {
		return false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type string `json:"type"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return false
	}
	return ev.Type == "content_block_delta" && ev.Delta.Type == kind && ev.Index == index
}

// IsDelta reports whether the line is a thinking_delta for this held block.
func (h *Hold) IsDelta(line []byte) bool {
	return IsDeltaFor(line, h.Index, "thinking_delta")
}

// IsRelated reports whether the line is part of the SSE envelope or the
// thinking block itself, and so must travel with a held content_block_start
// instead of ending the hold. Without this an "event: content_block_delta"
// line would ship the start before any thinking text was seen, and every
// summary would fall back to the generic one.
func IsRelated(line []byte, index int) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	if IsDeltaFor(line, index, "thinking_delta") || IsDeltaFor(line, index, "signature_delta") {
		return true
	}
	switch {
	case bytes.HasPrefix(line, []byte("event:")),
		bytes.HasPrefix(line, []byte("id:")),
		bytes.HasPrefix(line, []byte("retry:")),
		bytes.HasPrefix(line, []byte(":")):
		return true
	}
	return false
}

func DeltaText(line []byte) string {
	payload, ok := sseDataPayload(line)
	if !ok {
		return ""
	}
	var ev struct {
		Delta struct {
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return ""
	}
	return ev.Delta.Thinking
}

// sseDataPayload returns the JSON payload of a "data: ..." line.
func sseDataPayload(line []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
	if !ok {
		return nil, false
	}
	payload := bytes.TrimSpace(rest)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
		return nil, false
	}
	return payload, true
}

// WithSummary injects the collected summary into a held
// content_block_start line.
func WithSummary(line []byte, summary string) []byte {
	payload, ok := sseDataPayload(line)
	if !ok {
		return line
	}
	var ev map[string]any
	if json.Unmarshal(payload, &ev) != nil {
		return line
	}
	block, ok := ev["content_block"].(map[string]any)
	if !ok {
		return line
	}
	block["summaries"] = []map[string]string{{"type": "summary_text", "summary": summary}}
	out, err := json.Marshal(ev)
	if err != nil {
		return line
	}
	res := make([]byte, 0, len(out)+6)
	res = append(res, "data: "...)
	res = append(res, out...)
	return res
}
