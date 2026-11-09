package reqlog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestLineFormat(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(&buf, Config{})

	l.Request(RequestRecord{
		ID: "a1", Method: "POST", Path: "/v1/messages",
		ModelIn: "claude-GLM-5.3", ModelOut: "glm-5.3", Why: "picker",
		Status: 200, Duration: 1240 * time.Millisecond,
		In: 14, Out: 9, CacheRead: 100, Cost: 0.00042, Attempt: 2,
	})

	line := buf.String()
	wants := []string{
		`req=a1`,
		`POST /v1/messages`,
		`model=claude-GLM-5.3->glm-5.3`,
		`why=picker`,
		`status=200`,
		`dur=1.24s`,
		`in=14`,
		`out=9`,
		`cache_read=100`,
		`cost=0.000420`,
		`attempt=2`,
		`INFO  req=a1`,
	}
	for _, want := range wants {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q does not contain %q", line, want)
		}
	}

	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("log line %q is not exactly one newline-terminated line", line)
	}

	fields := strings.Fields(line)
	if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", fields[0], err)
	}
}

func TestRequestLineOmitsEmptySessionAndWhy(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(&buf, Config{})

	l.Request(RequestRecord{ID: "b2", Method: "GET", Path: "/health", ModelIn: "m", ModelOut: "m", Status: 200})

	line := buf.String()
	if !strings.Contains(line, `req=b2 GET /health`) {
		t.Errorf("log line %q lacks request coordinates", line)
	}
	if strings.Contains(line, "session=") {
		t.Errorf("log line %q has an empty session key", line)
	}
	if strings.Contains(line, "why=") {
		t.Errorf("log line %q has an empty why key", line)
	}
}

func TestMaskHeadersHidesCredentials(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer top-secret")
	h.Set("X-Api-Key", "sk-secret")
	h.Set("Content-Type", "application/json")

	got := MaskHeaders(h)

	if v := got["Authorization"]; len(v) != 1 || v[0] != "***" {
		t.Errorf("Authorization = %v, want [***]", v)
	}
	if v := got["X-Api-Key"]; len(v) != 1 || v[0] != "***" {
		t.Errorf("X-Api-Key = %v, want [***]", v)
	}
	if v := got["Content-Type"]; len(v) != 1 || v[0] != "application/json" {
		t.Errorf("Content-Type = %v, want [application/json]", v)
	}

	lower := http.Header{"authorization": {"Bearer top-secret"}}
	if v := MaskHeaders(lower)["authorization"]; len(v) != 1 || v[0] != "***" {
		t.Errorf("lower-case authorization = %v, want [***]", v)
	}

	if h.Get("Authorization") != "Bearer top-secret" {
		t.Errorf("MaskHeaders modified the input header")
	}
}

func TestCaptureJSONLWrittenOnlyWhenEnabled(t *testing.T) {
	cfg := Config{File: filepath.Join(t.TempDir(), "proxy.log"), MaxMB: 10, Keep: 5}
	capturePath := cfg.File + ".jsonl"

	var buf bytes.Buffer
	l := NewWithWriter(&buf, cfg)
	l.Request(RequestRecord{ID: "no-capture", Method: "POST", Path: "/v1/messages"})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
		t.Fatalf("capture file exists without Capture: %v", err)
	}

	l = NewWithWriter(&buf, cfg)
	l.Request(RequestRecord{
		ID: "a2", Method: "POST", Path: "/v1/messages",
		Capture: &CaptureRecord{
			RequestHeaders: map[string][]string{"Authorization": {"***"}},
			RequestBody:    "hello",
		},
	})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("capture file has %d lines, want 1: %q", len(lines), data)
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("capture line is not valid JSON: %v", err)
	}
	if entry["req"] != "a2" {
		t.Errorf("req = %v, want a2", entry["req"])
	}
	if entry["request_body"] != "hello" {
		t.Errorf("request_body = %v, want hello", entry["request_body"])
	}
	if _, ok := entry["ts"].(string); !ok {
		t.Errorf("ts missing or not a string: %v", entry["ts"])
	}
}

func TestCaptureBodyIsTruncated(t *testing.T) {
	cfg := Config{File: filepath.Join(t.TempDir(), "proxy.log"), MaxMB: 10, Keep: 5, MaxBodyBytes: 65536}
	body := strings.Repeat("x", 100000)

	// Truncation belongs to the caller: the handler cuts the body to
	// MaxBodyBytes from the config snapshot and sets Truncated itself.
	truncated := body[:int(cfg.MaxBodyBytes)]

	l := NewWithWriter(&bytes.Buffer{}, cfg)
	l.Request(RequestRecord{
		ID:      "big",
		Capture: &CaptureRecord{RequestBody: truncated, Truncated: true},
	})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(cfg.File + ".jsonl")
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	var entry struct {
		RequestBody string `json:"request_body"`
		Truncated   bool   `json:"truncated"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("capture line is not valid JSON: %v", err)
	}
	if len(entry.RequestBody) != 65536 {
		t.Errorf("captured body length = %d, want 65536", len(entry.RequestBody))
	}
	if !entry.Truncated {
		t.Errorf("truncated = false, want true")
	}
}

func TestCaptureWritesBodyVerbatim(t *testing.T) {
	cfg := Config{File: filepath.Join(t.TempDir(), "proxy.log"), MaxMB: 10, Keep: 5, MaxBodyBytes: 65536}
	body := strings.Repeat("x", 100000)

	l := NewWithWriter(&bytes.Buffer{}, cfg)
	l.Request(RequestRecord{ID: "raw", Capture: &CaptureRecord{RequestBody: body}})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(cfg.File + ".jsonl")
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	var entry struct {
		RequestBody string `json:"request_body"`
		Truncated   bool   `json:"truncated"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("capture line is not valid JSON: %v", err)
	}
	if len(entry.RequestBody) != len(body) {
		t.Errorf("captured body length = %d, want %d (logger must not truncate)", len(entry.RequestBody), len(body))
	}
	if entry.Truncated {
		t.Errorf("truncated = true, want false")
	}
}

func TestRotateShiftsAndDropsOldFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.log")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write(path, strings.Repeat("current", 20))
	write(path+".1", "one")
	write(path+".2", "two")
	write(path+".3", "three")

	if err := rotate(path, 10, 2); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}
	if got := read(path + ".1"); got != strings.Repeat("current", 20) {
		t.Errorf(".1 = %q, want current contents", got)
	}
	if got := read(path + ".2"); got != "one" {
		t.Errorf(".2 = %q, want %q", got, "one")
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf(".3 still exists after rotation with keep=2: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("active file still exists after rotation: %v", err)
	}
}

func TestRotationAppliesToBothFiles(t *testing.T) {
	t.Run("main log", func(t *testing.T) {
		cfg := Config{File: filepath.Join(t.TempDir(), "proxy.log"), MaxMB: 1, Keep: 1}
		l, err := NewLogger(cfg)
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}
		defer l.Close()

		if err := os.WriteFile(cfg.File, bytes.Repeat([]byte("x"), 1024*1024), 0o644); err != nil {
			t.Fatalf("prefill: %v", err)
		}
		l.Request(RequestRecord{ID: "r1", Method: "POST", Path: "/v1/messages"})

		if _, err := os.Stat(cfg.File + ".1"); err != nil {
			t.Fatalf("rotated main log: %v", err)
		}
		data, err := os.ReadFile(cfg.File)
		if err != nil {
			t.Fatalf("read main log: %v", err)
		}
		if !strings.Contains(string(data), "req=r1") {
			t.Errorf("active main log lacks the new request: %q", data)
		}
	})

	t.Run("capture log", func(t *testing.T) {
		cfg := Config{File: filepath.Join(t.TempDir(), "proxy.log"), MaxMB: 1, Keep: 1}
		capturePath := cfg.File + ".jsonl"
		if err := os.WriteFile(capturePath, bytes.Repeat([]byte("x"), 1024*1024), 0o644); err != nil {
			t.Fatalf("prefill: %v", err)
		}

		l := NewWithWriter(&bytes.Buffer{}, cfg)
		l.Request(RequestRecord{ID: "r2", Capture: &CaptureRecord{RequestBody: "hi"}})
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if _, err := os.Stat(capturePath + ".1"); err != nil {
			t.Fatalf("rotated capture log: %v", err)
		}
		data, err := os.ReadFile(capturePath)
		if err != nil {
			t.Fatalf("read capture log: %v", err)
		}
		if !strings.Contains(string(data), `"req":"r2"`) {
			t.Errorf("active capture log lacks the new entry: %q", data)
		}
	})
}
