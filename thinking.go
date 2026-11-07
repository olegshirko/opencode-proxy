package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	// thinkingSummaryMaxChars caps the summary the desktop app shows on the
	// collapsed thinking plate.
	thinkingSummaryMaxChars = 160
	// thinkingSummaryMaxBytes stops text accumulation: past this there is
	// still no complete meaningful line, so the block ships with a generic
	// summary rather than growing the buffer.
	thinkingSummaryMaxBytes = 256
	// thinkingSummaryFallback is used when no meaningful line was collected
	// within the wait window.
	thinkingSummaryFallback = "Thinking"
)

var markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
var listMarker = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)

// firstMeaningfulLine returns the first readable line of a thinking text,
// stripped of markdown decoration, or "" when there is none yet.
func firstMeaningfulLine(text string) string {
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

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// stripThinkingSummaries removes summary injections from the conversation
// history: they exist for the desktop UI and must not leak back upstream.
func stripThinkingSummaries(data map[string]any) {
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

// addThinkingSummariesJSON injects a one-line summary into every thinking
// block of a non-streaming response, so the app renders it collapsed.
func addThinkingSummariesJSON(body []byte) []byte {
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
		sum := firstMeaningfulLine(text)
		if sum == "" {
			sum = thinkingSummaryFallback
		}
		blk["summaries"] = []map[string]string{{"type": "summary_text", "summary": truncateRunes(sum, thinkingSummaryMaxChars)}}
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

// thinkingHold is the state of a thinking block whose content_block_start is
// being held back while its summary is collected. Only the start event is
// delayed; deltas and stop keep their order, they are buffered with it.
type thinkingHold struct {
	wait    time.Duration
	started time.Time
	line    []byte
	index   int
	deltas  [][]byte
	text    bytes.Buffer
	// timer releases the hold when the model pauses between thinking bursts;
	// without it a long gap would keep the block from reaching the client.
	timer *time.Timer
}

func maybeHoldThinkingStart(line []byte, wait time.Duration) *thinkingHold {
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
	return &thinkingHold{
		wait:    wait,
		started: time.Now(),
		line:    append([]byte(nil), line...),
		index:   ev.Index,
	}
}

// isDeltaFor reports whether the line is a content_block_delta of the given
// delta type for the held block index.
func isDeltaFor(line []byte, index int, kind string) bool {
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

func isThinkingDeltaFor(line []byte, index int) bool {
	return isDeltaFor(line, index, "thinking_delta")
}

// isAmongHeldLines reports whether the line is part of the SSE envelope or the
// thinking block itself, and so must travel with a held content_block_start
// instead of ending the hold. Without this an "event: content_block_delta"
// line would ship the start before any thinking text was seen, and every
// summary would fall back to the generic one.
func isAmongHeldLines(line []byte, index int) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	if isThinkingDeltaFor(line, index) || isDeltaFor(line, index, "signature_delta") {
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

func thinkingDeltaText(line []byte) string {
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

// withThinkingSummary injects the collected summary into a held
// content_block_start line.
func withThinkingSummary(line []byte, summary string) []byte {
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
