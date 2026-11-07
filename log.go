package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RequestRecord is one completed proxy request. Capture is nil unless traffic
// capture is enabled for the request.
type RequestRecord struct {
	ID        string
	Method    string
	Path      string
	Session   string
	ModelIn   string
	ModelOut  string
	Why       string
	Status    int
	Attempt   int
	Duration  time.Duration
	In        float64
	Out       float64
	CacheRead float64
	Cost      float64
	Allowance float64
	Aborted   bool
	Capture   *CaptureRecord
}

// CaptureRecord holds the traffic captured for one request. Truncation is
// applied by the caller, which knows the configured body limit; the logger
// writes the record as given.
type CaptureRecord struct {
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	RequestBody     string              `json:"request_body,omitempty"`
	ResponseBody    string              `json:"response_body,omitempty"`
	Truncated       bool                `json:"truncated,omitempty"`
}

// Logger writes one line per request and, when capture is present, one JSON
// object per request to <file>.jsonl. Both files rotate by size independently.
// It is safe for concurrent use.
type Logger struct {
	mu          sync.Mutex
	cfg         LogConfig
	main        io.Writer
	mainFile    *os.File
	captureFile *os.File
}

// NewLogger opens the main log file, creating its directory if needed.
func NewLogger(cfg LogConfig) (*Logger, error) {
	if cfg.File == "" {
		return nil, errors.New("log: file path is empty")
	}
	if dir := filepath.Dir(cfg.File); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("log: create dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("log: open %s: %w", cfg.File, err)
	}
	return &Logger{cfg: cfg, main: f, mainFile: f}, nil
}

// newLoggerWithWriter builds a logger that writes the main log to w without
// rotation. Capture still goes to <cfg.File>.jsonl when cfg.File is set.
func newLoggerWithWriter(w io.Writer, cfg LogConfig) *Logger {
	return &Logger{cfg: cfg, main: w}
}

// Request writes the request line and, if rec.Capture is set, its capture
// record. Write failures are ignored: logging must never break a request.
func (l *Logger) Request(rec RequestRecord) {
	now := time.Now()
	line := formatRequestLine(now, rec)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.rotateMain()
	_, _ = io.WriteString(l.main, line)

	if rec.Capture != nil {
		l.writeCapture(now, rec)
	}
}

// Close closes the log files. It is safe to call more than once.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var errs []error
	if l.mainFile != nil {
		errs = append(errs, l.mainFile.Close())
		l.mainFile = nil
		l.main = io.Discard
	}
	if l.captureFile != nil {
		errs = append(errs, l.captureFile.Close())
		l.captureFile = nil
	}
	return errors.Join(errs...)
}

func formatRequestLine(now time.Time, rec RequestRecord) string {
	var b strings.Builder
	b.WriteString(now.Format(time.RFC3339))
	b.WriteString(" INFO  req=")
	b.WriteString(rec.ID)
	b.WriteByte(' ')
	b.WriteString(rec.Method)
	b.WriteByte(' ')
	b.WriteString(rec.Path)
	if rec.Session != "" {
		b.WriteString(" session=")
		b.WriteString(rec.Session)
	}
	b.WriteString(" model=")
	b.WriteString(rec.ModelIn)
	b.WriteString("->")
	b.WriteString(rec.ModelOut)
	if rec.Why != "" {
		b.WriteString(" why=")
		b.WriteString(rec.Why)
	}
	fmt.Fprintf(&b, " status=%d dur=%.2fs in=%d out=%d cache_read=%d cost=%f attempt=%d\n",
		rec.Status, rec.Duration.Seconds(), int64(rec.In), int64(rec.Out),
		int64(rec.CacheRead), rec.Cost, rec.Attempt)
	return b.String()
}

// captureEntry is the JSONL shape: request coordinates plus the capture.
type captureEntry struct {
	Req string `json:"req"`
	TS  string `json:"ts"`
	CaptureRecord
}

func (l *Logger) writeCapture(now time.Time, rec RequestRecord) {
	w, err := l.captureWriter()
	if err != nil {
		return
	}
	entry := captureEntry{
		Req:           rec.ID,
		TS:            now.Format(time.RFC3339),
		CaptureRecord: *rec.Capture,
	}
	_ = json.NewEncoder(w).Encode(entry)
}

// captureWriter returns the capture file, rotating it first when it reached
// the size limit, and opening it on first use.
func (l *Logger) captureWriter() (io.Writer, error) {
	if l.cfg.File == "" {
		return nil, errors.New("log: capture requires a log file path")
	}
	path := l.cfg.File + ".jsonl"

	if l.captureFile != nil {
		if info, err := l.captureFile.Stat(); l.cfg.MaxMB > 0 && err == nil && info.Size() >= l.maxBytes() {
			l.captureFile.Close()
			l.captureFile = nil
		}
		if l.captureFile != nil {
			return l.captureFile, nil
		}
	}

	if err := rotate(path, l.maxBytes(), l.cfg.Keep); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	l.captureFile = f
	return f, nil
}

// rotateMain moves the main log aside before a write that would grow it past
// the configured size.
func (l *Logger) rotateMain() {
	if l.mainFile == nil || l.cfg.MaxMB <= 0 {
		return
	}
	info, err := l.mainFile.Stat()
	if err != nil || info.Size() < l.maxBytes() {
		return
	}

	l.mainFile.Close()
	l.mainFile = nil
	_ = rotate(l.cfg.File, l.maxBytes(), l.cfg.Keep)

	f, err := os.OpenFile(l.cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		l.main = io.Discard
		return
	}
	l.mainFile = f
	l.main = f
}

func (l *Logger) maxBytes() int64 {
	return int64(l.cfg.MaxMB) * 1024 * 1024
}

// rotate moves path to path+".1" when it has reached maxBytes, shifting older
// backups up and deleting those beyond keep. A missing file is not an error;
// maxBytes <= 0 disables rotation.
func rotate(path string, maxBytes int64, keep int) error {
	if maxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxBytes {
		return nil
	}
	if keep < 0 {
		keep = 0
	}

	// Remove leftovers from an earlier, larger keep.
	backups, err := filepath.Glob(path + ".*")
	if err != nil {
		return err
	}
	for _, backup := range backups {
		n, err := strconv.Atoi(strings.TrimPrefix(backup, path+"."))
		if err == nil && n > keep {
			if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}

	if keep == 0 {
		return os.Remove(path)
	}
	if err := os.Remove(fmt.Sprintf("%s.%d", path, keep)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", path, i)
		to := fmt.Sprintf("%s.%d", path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(path, path+".1")
}

// maskHeaders copies h, replacing the values of credential-bearing headers
// with "***". The header names are compared case-insensitively.
func maskHeaders(h http.Header) map[string][]string {
	masked := make(map[string][]string, len(h))
	for name, values := range h {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "x-api-key") {
			masked[name] = []string{"***"}
			continue
		}
		masked[name] = append([]string(nil), values...)
	}
	return masked
}
