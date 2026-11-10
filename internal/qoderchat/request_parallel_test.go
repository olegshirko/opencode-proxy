package qoderchat

import (
	"encoding/json"
	"testing"
)

func TestTranslateRequestParallelToolResults(t *testing.T) {
	// Claude Code fires tools in parallel and replays the whole transcript:
	// one user message carrying several tool_result blocks with different ids
	// is the canonical shape. It must map to one role:"tool" message each.
	in := []byte(`{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":"run"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"tu_1","name":"a","input":{}},
			{"type":"tool_use","id":"tu_2","name":"b","input":{}}
		]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":"one"},
			{"type":"tool_result","tool_use_id":"tu_2","content":"two"}
		]}
	]}`)
	got, err := TranslateRequest(in, "m", RequestMeta{})
	if err != nil {
		t.Fatalf("parallel tool_results rejected: %v", err)
	}
	var q struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &q); err != nil {
		t.Fatal(err)
	}
	var toolMsgs []int
	for i, m := range q.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, i)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("want 2 tool messages, got %d (%s)", len(toolMsgs), got)
	}
	if q.Messages[toolMsgs[0]].ToolCallID != "tu_1" || q.Messages[toolMsgs[1]].ToolCallID != "tu_2" {
		t.Fatalf("tool_call_id mapping wrong: %+v", q.Messages)
	}
}
