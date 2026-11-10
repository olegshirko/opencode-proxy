package qoderchat

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const openAIStreamFixture = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking hard\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"MSK\\\"}\"}}]}}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n" +
	"\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n" +
	"\n" +
	"data: [DONE]\n\n"

type sseEvent struct {
	Event string
	Data  map[string]any
}

func parseSSE(t *testing.T, out string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, chunk := range strings.Split(out, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		ev := sseEvent{Data: map[string]any{}}
		for _, line := range strings.Split(chunk, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev.Event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.Data); err != nil {
					t.Fatalf("bad data line %q: %v", line, err)
				}
			}
		}
		events = append(events, ev)
	}
	return events
}

func TestTranslateStreamGolden(t *testing.T) {
	var b strings.Builder
	usage, err := TranslateStream(strings.NewReader(openAIStreamFixture), &b, "qoder-model")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	events := parseSSE(t, b.String())

	var names []string
	for _, ev := range events {
		names = append(names, ev.Event)
	}
	wantNames := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // text "Hello"
		"content_block_start", "content_block_delta", "content_block_stop", // thinking
		"content_block_start", "content_block_delta", "content_block_stop", // text " world"
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // tool_use: 2 arg fragments
		"message_delta", "message_stop",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("event names:\n got %v\nwant %v", names, wantNames)
	}

	// message_start
	ms := events[0].Data
	if ms["type"] != "message_start" {
		t.Fatalf("first event data: %v", ms)
	}
	msg := ms["message"].(map[string]any)
	if msg["id"] != "c1" || msg["model"] != "qoder-model" || msg["role"] != "assistant" {
		t.Fatalf("message_start content: %v", msg)
	}

	// text block: Hello (index 0)
	bs := events[1].Data
	if bs["index"] != float64(0) || bs["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("text block start: %v", bs)
	}
	if d := events[2].Data; d["delta"].(map[string]any)["text"] != "Hello" {
		t.Fatalf("text delta: %v", d)
	}

	// thinking block (index 1)
	if cb := events[4].Data["content_block"].(map[string]any); cb["type"] != "thinking" {
		t.Fatalf("thinking start: %v", events[4].Data)
	}
	if d := events[5].Data; d["delta"].(map[string]any)["type"] != "thinking_delta" {
		t.Fatalf("thinking delta: %v", d)
	}

	// tool_use block (index 3): accumulated input_json_delta = {"city":"MSK"}
	tu := events[10].Data
	if tu["index"] != float64(3) {
		t.Fatalf("tool block index: %v", tu)
	}
	cb := tu["content_block"].(map[string]any)
	if cb["type"] != "tool_use" || cb["id"] != "call_1" || cb["name"] != "get_weather" {
		t.Fatalf("tool block: %v", cb)
	}
	if d := events[11].Data["delta"].(map[string]any); d["partial_json"] != "{\"city\":" {
		t.Fatalf("tool args fragment 1: %v", d)
	}
	if d := events[12].Data["delta"].(map[string]any); d["partial_json"] != "\"MSK\"}" {
		t.Fatalf("tool args fragment 2: %v", d)
	}

	// message_delta: stop_reason tool_use + usage
	md := events[len(events)-2].Data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason: %v", md)
	}
	u := md["usage"].(map[string]any)
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(5) {
		t.Fatalf("message_delta usage: %v", u)
	}

	// returned usage includes cache read
	if usage != (Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3}) {
		t.Fatalf("returned usage: %+v", usage)
	}
}

func TestTranslateStreamStopReasons(t *testing.T) {
	cases := map[string]string{
		"stop":   "end_turn",
		"length": "max_tokens",
	}
	for finish, want := range cases {
		in := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"" + finish + "\"}]}\n\ndata: [DONE]\n\n"
		var b strings.Builder
		if _, err := TranslateStream(strings.NewReader(in), &b, "m"); err != nil {
			t.Fatalf("%s: %v", finish, err)
		}
		events := parseSSE(t, b.String())
		md := events[len(events)-2].Data
		if got := md["delta"].(map[string]any)["stop_reason"]; got != want {
			t.Fatalf("finish %s: stop_reason %v, want %s", finish, got, want)
		}
	}
}

func TestTranslateStreamUsageTotals(t *testing.T) {
	in := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n" +
		"data: [DONE]\n\n"
	var b strings.Builder
	usage, err := TranslateStream(strings.NewReader(in), &b, "m")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 20 || usage.CacheReadTokens != 80 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestCollectNonStreamToolCall(t *testing.T) {
	body, usage, err := CollectNonStream(strings.NewReader(openAIStreamFixture), "qoder-model")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("body: %v\n%s", err, body)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["stop_reason"] != "tool_use" {
		t.Fatalf("message: %v", msg)
	}
	content := msg["content"].([]any)
	if len(content) != 4 { // text "Hello", thinking, text " world", tool_use
		t.Fatalf("content blocks: %d (%v)", len(content), content)
	}
	tu := content[3].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "get_weather" {
		t.Fatalf("tool_use block: %v", tu)
	}
	input := tu["input"].(map[string]any)
	if input["city"] != "MSK" {
		t.Fatalf("tool input: %v", input)
	}
	if usage.OutputTokens != 5 {
		t.Fatalf("usage: %+v", usage)
	}
}
