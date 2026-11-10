package qoderchat

import (
	"encoding/json"
)

// EstimateTokens is the proxy's local stand-in for count_tokens on Qoder
// routes: Qoder has no token-count endpoint. It is a chars/4 heuristic over
// text content plus a flat per-tool allowance — an approximation, never an
// error: the client hangs if count_tokens fails, so a malformed body still
// gets a floor from the raw byte length.
func EstimateTokens(anthropic []byte) (int, error) {
	var in anthropicRequest
	if err := json.Unmarshal(anthropic, &in); err != nil {
		return estimateFromBytes(anthropic), nil
	}

	total := 0
	total += textTokens(systemText(in.System))
	for _, tl := range in.Tools {
		total += 40 // flat allowance for a tool definition
		total += textTokens(tl.Description)
	}
	for _, msg := range in.Messages {
		total += messageTokens(msg)
	}
	if total <= 0 {
		total = estimateFromBytes(anthropic)
	}
	return total, nil
}

func estimateFromBytes(raw []byte) int {
	n := len(raw) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func messageTokens(msg anthropicMessage) int {
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return textTokens(s)
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return estimateFromBytes(msg.Content)
	}
	total := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			total += textTokens(b.Text)
		case "image":
			total += 1600 // ~fixed cost of one image
		case "tool_use":
			total += textTokens(string(b.Input)) + 20
		case "tool_result":
			total += textTokens(string(b.Content))
		case "thinking", "redacted_thinking":
			total += textTokens(b.Thinking)
		}
	}
	return total
}

func textTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}
