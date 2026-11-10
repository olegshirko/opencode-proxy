package qoderchat

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func load(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func canon(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad json: %v\n%s", err, raw)
	}
	return v
}

func TestTranslateRequestGolden(t *testing.T) {
	in := load(t, "testdata/anthropic_request.json")
	want := load(t, "testdata/qoder_request.json")

	got, err := TranslateRequest(in, "qoder-model", RequestMeta{RequestID: "req-1", SessionID: "sess-1", ClientType: "opencode-proxy"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !reflect.DeepEqual(canon(t, got), canon(t, want)) {
		t.Fatalf("golden mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestTranslateRequestUnsupportedToolChoice(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":1,"tool_choice":{"type":"hello"},"messages":[{"role":"user","content":"x"}]}`)
	_, err := TranslateRequest(in, "m", RequestMeta{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestTranslateRequestToolChoiceMapping(t *testing.T) {
	tests := []struct {
		choice string
		want   string
	}{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"tool","name":"get_weather"}`, `{"type":"function","function":{"name":"get_weather"}}`},
	}
	for _, tc := range tests {
		in := []byte(`{"model":"m","max_tokens":1,"tool_choice":` + tc.choice + `,"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"x"}]}`)
		got, err := TranslateRequest(in, "m", RequestMeta{})
		if err != nil {
			t.Fatalf("choice %s: %v", tc.choice, err)
		}
		var q map[string]any
		if err := json.Unmarshal(got, &q); err != nil {
			t.Fatal(err)
		}
		var wantVal any
		if err := json.Unmarshal([]byte(tc.want), &wantVal); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(q["tool_choice"], wantVal) {
			t.Fatalf("choice %s: tool_choice = %#v, want %#v", tc.choice, q["tool_choice"], wantVal)
		}
	}
}

func TestEstimateTokensTypicalBody(t *testing.T) {
	// ~40k chars of text plus tools: estimate must land near chars/4 and never
	// error on a valid body, regardless of the model name.
	var b strings.Builder
	b.WriteString(`{"model":"claude-x","max_tokens":10,"tools":[{"name":"t","description":"`)
	b.WriteString(strings.Repeat("d", 120))
	b.WriteString(`","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"`)
	b.WriteString(strings.Repeat("a", 40_000))
	b.WriteString(`"}]}`)
	in := []byte(b.String())

	n, err := EstimateTokens(in)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	want := 40_000/4 + 40
	if n < want/2 || n > want*2 {
		t.Fatalf("estimate %d, want within 2x of %d", n, want)
	}
}

func TestEstimateTokensMalformedBody(t *testing.T) {
	// count_tokens must always answer on the proxy path; a body that does not
	// parse still gets a sane floor instead of an error.
	n, err := EstimateTokens([]byte(`{"messages":"not-an-array"`))
	if err != nil || n <= 0 {
		t.Fatalf("estimate on malformed body: n=%d err=%v", n, err)
	}
}
