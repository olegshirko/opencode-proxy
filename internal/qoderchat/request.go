// Package qoderchat translates between the Anthropic Messages API the proxy's
// clients speak and Qoder's OpenAI-compatible chat completions API. Request
// direction lives here; stream and non-stream response direction in
// response.go. The mapping follows the request builder observed in Qoder's
// worker (design spec, "Инференс").
package qoderchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RequestMeta carries the proxy's correlation ids into Qoder's metadata block.
type RequestMeta struct {
	RequestID  string // X-Request-ID
	SessionID  string // X-Session-ID
	ClientType string
}

// ErrUnsupported marks a client request feature the Qoder API has no mapping
// for. The proxy surfaces it as 400 rather than mangling the upstream call.
var ErrUnsupported = errors.New("qoderchat: unsupported request feature")

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	System        json.RawMessage    `json:"system"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Metadata      json.RawMessage    `json:"metadata"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

// qoderRequest mirrors the fields TranslateRequest emits. Everything else
// stays out: fields the client did not send are not sent upstream either.
type qoderRequest struct {
	Model         string           `json:"model"`
	Messages      []qoderMessage   `json:"messages"`
	Stream        bool             `json:"stream"`
	StreamOptions *qoderStreamOpts `json:"stream_options,omitempty"`
	Metadata      qoderMetadata    `json:"metadata"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	Stop          []string         `json:"stop,omitempty"`
	Tools         []qoderTool      `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty"`
}

type qoderStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type qoderMetadata struct {
	Context qoderContext `json:"context"`
}

type qoderContext struct {
	RequestID  string `json:"request_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ClientType string `json:"client_type,omitempty"`
}

type qoderTool struct {
	Type     string       `json:"type"`
	Function qoderToolDef `json:"function"`
}

type qoderToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type qoderMessage struct {
	Role       string          `json:"role"`
	Content    any             `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []qoderToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type qoderToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	} `json:"function"`
}

// contentPart is one block of an OpenAI-style multimodal content array.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// TranslateRequest converts one Anthropic Messages body to the Qoder chat
// completions body. The model argument replaces the client's model name with
// the resolved upstream id; the caller decides streaming separately, so the
// translated body always asks for a stream with usage.
func TranslateRequest(anthropic []byte, model string, meta RequestMeta) ([]byte, error) {
	var in anthropicRequest
	if err := json.Unmarshal(anthropic, &in); err != nil {
		return nil, fmt.Errorf("%w: bad anthropic body: %v", ErrUnsupported, err)
	}

	out := qoderRequest{
		Model:         model,
		Stream:        true,
		StreamOptions: &qoderStreamOpts{IncludeUsage: true},
		Metadata:      qoderMetadata{Context: qoderContext{RequestID: meta.RequestID, SessionID: meta.SessionID, ClientType: meta.ClientType}},
		MaxTokens:     in.MaxTokens,
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		Stop:          in.StopSequences,
	}

	if sys := systemText(in.System); sys != "" {
		out.Messages = append(out.Messages, qoderMessage{Role: "system", Content: sys})
	}

	for i, msg := range in.Messages {
		qm, err := translateMessage(msg)
		if err != nil {
			return nil, err
		}
		// A tool_result belongs to the tool call before it; the proxy never
		// sees one without a matching assistant tool_use.
		if qm.Role == "tool" && i == 0 {
			return nil, fmt.Errorf("%w: tool result before any assistant message", ErrUnsupported)
		}
		out.Messages = append(out.Messages, qm)
	}

	for _, tl := range in.Tools {
		out.Tools = append(out.Tools, qoderTool{
			Type: "function",
			Function: qoderToolDef{
				Name:        tl.Name,
				Description: tl.Description,
				Parameters:  rawOrEmpty(tl.InputSchema),
			},
		})
	}

	if len(in.ToolChoice) > 0 {
		choice, err := translateToolChoice(in.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = choice
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func translateMessage(msg anthropicMessage) (qoderMessage, error) {
	// Plain string content: 1:1.
	var contentStr string
	if err := json.Unmarshal(msg.Content, &contentStr); err == nil {
		return qoderMessage{Role: msg.Role, Content: contentStr}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return qoderMessage{}, fmt.Errorf("%w: message content: %v", ErrUnsupported, err)
	}

	if msg.Role == "user" && len(blocks) > 0 && blocks[0].Type == "tool_result" {
		return toolResultMessage(blocks)
	}

	qm := qoderMessage{Role: msg.Role}
	var parts []contentPart
	var texts []string
	var toolCalls []qoderToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
			parts = append(parts, contentPart{Type: "text", Text: b.Text})
		case "image":
			src := b.Source
			if src.Type != "base64" {
				return qoderMessage{}, fmt.Errorf("%w: image source %q", ErrUnsupported, src.Type)
			}
			parts = append(parts, contentPart{
				Type: "image_url",
				ImageURL: &struct {
					URL string `json:"url"`
				}{URL: "data:" + src.MediaType + ";base64," + src.Data},
			})
		case "thinking", "redacted_thinking":
			// Client-echoed reasoning has no upstream counterpart; drop it.
			continue
		case "tool_use":
			toolCalls = append(toolCalls, qoderToolCall{
				ID:   b.ID,
				Type: "function",
			})
			toolCalls[len(toolCalls)-1].Function.Name = b.Name
			toolCalls[len(toolCalls)-1].Function.Arguments = rawOrNull(b.Input)
		default:
			return qoderMessage{}, fmt.Errorf("%w: content block %q", ErrUnsupported, b.Type)
		}
	}

	if len(toolCalls) > 0 {
		qm.ToolCalls = toolCalls
	}
	switch {
	case len(parts) == 1 && parts[0].Type == "text":
		qm.Content = parts[0].Text
	case len(parts) > 0:
		qm.Content = parts
	case len(texts) > 0:
		qm.Content = strings.Join(texts, "\n\n")
	}
	return qm, nil
}

func toolResultMessage(blocks []anthropicBlock) (qoderMessage, error) {
	qm := qoderMessage{Role: "tool"}
	var texts []string
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return qoderMessage{}, fmt.Errorf("%w: tool_result block mixed with %q", ErrUnsupported, b.Type)
		}
		if qm.ToolCallID == "" {
			qm.ToolCallID = b.ToolUseID
		} else if qm.ToolCallID != b.ToolUseID {
			return qoderMessage{}, fmt.Errorf("%w: tool_result blocks with different tool_use_id", ErrUnsupported)
		}
		switch {
		case b.Content != nil:
			var s string
			if err := json.Unmarshal(b.Content, &s); err == nil {
				texts = append(texts, s)
			} else {
				var inner []anthropicBlock
				if err := json.Unmarshal(b.Content, &inner); err != nil {
					return qoderMessage{}, fmt.Errorf("%w: tool_result content: %v", ErrUnsupported, err)
				}
				for _, ib := range inner {
					if ib.Type == "text" {
						texts = append(texts, ib.Text)
					}
				}
			}
		case b.IsError:
			texts = append(texts, "tool error")
		}
	}
	qm.Content = strings.Join(texts, "\n\n")
	return qm, nil
}

func translateToolChoice(raw json.RawMessage) (any, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("%w: tool_choice: %v", ErrUnsupported, err)
	}
	switch tc.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "tool":
		if tc.Name == "" {
			return nil, fmt.Errorf("%w: tool_choice without name", ErrUnsupported)
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}, nil
	default:
		return nil, fmt.Errorf("%w: tool_choice type %q", ErrUnsupported, tc.Type)
	}
}

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func rawOrEmpty(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{}
	}
	return v
}

func rawOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}
