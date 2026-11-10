package qoderchat

import "encoding/json"

// anthropicBlock is one entry of an Anthropic content array. Only the fields
// the translator needs are decoded.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text"`

	// image
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// thinking
	Thinking string `json:"thinking"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string or []block
	IsError   bool            `json:"is_error"`
}
