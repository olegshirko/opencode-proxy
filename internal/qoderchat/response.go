package qoderchat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Usage is the token accounting extracted from the final usage chunk.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
}

// openAIChunk is one data: payload of the upstream stream.
type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// outSink receives the assembled Anthropic events. SSE mode writes them as
// server-sent events; collect mode (out == nil) records structured blocks.
type outSink interface {
	messageStart(id, model string)
	blockStart(index int, block map[string]any)
	blockDelta(index int, delta map[string]any)
	blockStop(index int)
	messageDelta(stopReason string, u Usage)
	messageStop()
}

type sseOut struct {
	w io.Writer
}

func (s *sseOut) write(name string, payload any) {
	buf, _ := json.Marshal(payload)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, buf)
}
func (s *sseOut) messageStart(id, model string) {
	s.write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}
func (s *sseOut) blockStart(index int, block map[string]any) {
	s.write("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
}
func (s *sseOut) blockDelta(index int, delta map[string]any) {
	s.write("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
}
func (s *sseOut) blockStop(index int) {
	s.write("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}
func (s *sseOut) messageDelta(stopReason string, u Usage) {
	s.write("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens},
	})
}
func (s *sseOut) messageStop() {
	s.write("message_stop", map[string]any{"type": "message_stop"})
}

// collected is the final Anthropic message assembled in non-stream mode.
type collected struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []any          `json:"content"`
	StopReason string         `json:"stop_reason"`
	StopSeq    *string        `json:"stop_sequence"`
	Usage      map[string]any `json:"usage"`
}

// assembler turns a sequence of OpenAI chunks into Anthropic events. It opens
// and closes content blocks as the upstream switches between reasoning, text
// and tool-call fragments.
type assembler struct {
	model    string
	out      outSink // nil in collect mode
	msgID    string
	started  bool
	blockIdx int
	open     string // "", "text", "thinking", "tool_use"

	toolSeen  map[int]bool
	toolArgs  map[int]*strings.Builder
	toolID    map[int]string
	toolName  map[int]string
	toolOrder []int

	stopReason string
	usage      Usage
	usageSeen  bool

	// collect-mode accumulators (index-aligned with blockIdx)
	blocks        []map[string]any
	textBuf       strings.Builder
	collectedBody []byte
}

func newAssembler(model string, out outSink) *assembler {
	return &assembler{
		model:    model,
		out:      out,
		toolSeen: map[int]bool{},
		toolArgs: map[int]*strings.Builder{},
		toolID:   map[int]string{},
		toolName: map[int]string{},
	}
}

func (a *assembler) ensureStart(id, _ string) {
	if a.started {
		return
	}
	a.started = true
	if id != "" {
		a.msgID = id
	}
	// a.model stays the caller-provided name: message_start reports the model
	// the proxy resolved, not the upstream echo.
	if a.out != nil {
		a.out.messageStart(a.msgID, a.model)
	}
}

// closeBlock ends the open block, flushing its buffered text in collect mode.
func (a *assembler) closeBlock() {
	if a.open == "" {
		return
	}
	switch a.open {
	case "text":
		if a.out == nil {
			a.blocks[len(a.blocks)-1]["text"] = a.textBuf.String()
		}
	case "thinking":
		if a.out == nil {
			a.blocks[len(a.blocks)-1]["thinking"] = a.blocks[len(a.blocks)-1]["_buf"].(string)
			delete(a.blocks[len(a.blocks)-1], "_buf")
		}
	case "tool_use":
		if a.out == nil {
			last := a.toolOrder[len(a.toolOrder)-1]
			a.blocks[len(a.blocks)-1]["input"] = parseToolArgs(a.toolArgs[last].String())
		}
	}
	if a.out != nil {
		a.out.blockStop(a.blockIdx)
	}
	a.blockIdx++
	a.open = ""
}

func (a *assembler) chunk(c openAIChunk) {
	a.ensureStart(c.ID, c.Model)
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.ReasoningContent != "" {
			if a.open != "thinking" {
				a.closeBlock()
				a.open = "thinking"
				if a.out != nil {
					a.out.blockStart(a.blockIdx, map[string]any{"type": "thinking", "thinking": ""})
				} else {
					a.blocks = append(a.blocks, map[string]any{"type": "thinking", "_buf": ""})
				}
			}
			if a.out != nil {
				a.out.blockDelta(a.blockIdx, map[string]any{"type": "thinking_delta", "thinking": d.ReasoningContent})
			} else {
				b := a.blocks[len(a.blocks)-1]
				b["_buf"] = b["_buf"].(string) + d.ReasoningContent
			}
		}
		if d.Content != "" {
			if a.open != "text" {
				a.closeBlock()
				a.open = "text"
				a.textBuf.Reset()
				if a.out != nil {
					a.out.blockStart(a.blockIdx, map[string]any{"type": "text", "text": ""})
				} else {
					a.blocks = append(a.blocks, map[string]any{"type": "text"})
				}
			}
			if a.out != nil {
				a.out.blockDelta(a.blockIdx, map[string]any{"type": "text_delta", "text": d.Content})
			} else {
				a.textBuf.WriteString(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			if !a.toolSeen[tc.Index] {
				a.closeBlock()
				a.toolSeen[tc.Index] = true
				a.toolOrder = append(a.toolOrder, tc.Index)
				a.toolArgs[tc.Index] = &strings.Builder{}
				a.toolID[tc.Index] = tc.ID
				a.toolName[tc.Index] = tc.Function.Name
				a.open = "tool_use"
				if a.out != nil {
					a.out.blockStart(a.blockIdx, map[string]any{
						"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{},
					})
				} else {
					a.blocks = append(a.blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name})
				}
			}
			if tc.Function.Arguments != "" {
				a.toolArgs[tc.Index].WriteString(tc.Function.Arguments)
				if a.out != nil {
					a.out.blockDelta(a.blockIdx, map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments})
				}
			}
		}
		if ch.FinishReason != nil {
			a.stopReason = mapStopReason(*ch.FinishReason)
		}
	}
	if c.Usage != nil {
		a.usage.InputTokens = c.Usage.PromptTokens
		a.usage.OutputTokens = c.Usage.CompletionTokens
		if c.Usage.PromptTokensDetails != nil {
			a.usage.CacheReadTokens = c.Usage.PromptTokensDetails.CachedTokens
		}
		a.usageSeen = true
	}
}

// finish closes the stream: final block stop, message_delta with usage, and
// in collect mode the assembled message.
func (a *assembler) finish() (Usage, error) {
	if a.started {
		a.closeBlock()
	}
	if !a.usageSeen {
		a.usage = Usage{}
	}
	if a.out != nil {
		stop := a.stopReason
		if stop == "" {
			stop = "end_turn"
		}
		a.out.messageDelta(stop, a.usage)
		a.out.messageStop()
		return a.usage, nil
	}
	stop := a.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	content := make([]any, len(a.blocks))
	for i, b := range a.blocks {
		content[i] = b
	}
	msg := collected{
		ID:         a.msgID,
		Type:       "message",
		Role:       "assistant",
		Model:      a.model,
		Content:    content,
		StopReason: stop,
		Usage: map[string]any{
			"input_tokens": a.usage.InputTokens, "output_tokens": a.usage.OutputTokens,
		},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return a.usage, err
	}
	a.collectedBody = body
	return a.usage, nil
}

func mapStopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func parseToolArgs(raw string) map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v == nil {
		return map[string]any{}
	}
	return v
}

// TranslateStream reads Qoder's OpenAI-style SSE stream and writes Anthropic
// streaming events to w. It returns the usage from the final chunk.
func TranslateStream(src io.Reader, w io.Writer, model string) (Usage, error) {
	a := newAssembler(model, &sseOut{w: w})
	if err := pump(src, a); err != nil {
		return Usage{}, err
	}
	return a.finish()
}

// CollectNonStream does the same translation but accumulates the result into
// one Anthropic Messages JSON body, for clients that asked stream:false.
func CollectNonStream(src io.Reader, model string) ([]byte, Usage, error) {
	a := newAssembler(model, nil)
	if err := pump(src, a); err != nil {
		return nil, Usage{}, err
	}
	usage, err := a.finish()
	return a.collectedBody, usage, err
}

func pump(src io.Reader, a *assembler) error {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if string(payload) == "[DONE]" {
			break
		}
		var c openAIChunk
		if err := json.Unmarshal(payload, &c); err != nil {
			// Keep-alives and comments ride the same stream; ignore junk lines.
			continue
		}
		a.chunk(c)
	}
	return sc.Err()
}
