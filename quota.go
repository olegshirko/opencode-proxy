package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// usageRetention keeps a little longer than the longest quota window, so the
// monthly sum and the rolling week both stay exact without unbounded growth.
const usageRetention = 35 * 24 * time.Hour

const (
	usageFileVersion = 1

	fiveHourWindowLength = 5 * time.Hour
)

// LimitsConfig maps the OpenCode Go limits onto the three windows the app's
// Limits panel shows: a 5-hour window that starts on first use, a weekly
// window, and a monthly window. Go defines them as shares of the monthly
// amount: 5 hours is 20% of it, a week 50%.
// https://opencode.ai/docs/console/go
//
// MonthlyUSD == 0 disables the whole feature: no quota headers are emitted.
type LimitsConfig struct {
	// MonthlyUSD is the dollar limit of the monthly window, which is not the
	// plan price: a $10 Go plan measures up to about $15 of usage.
	MonthlyUSD       float64 `json:"monthly_usd"`
	FiveHourFraction float64 `json:"five_hour_fraction"`
	WeeklyFraction   float64 `json:"weekly_fraction"`
	WarningFraction  float64 `json:"warning_fraction"`
	// MonthStart anchors the monthly window when it does not follow the
	// calendar month. The panel shows when it resets; that time minus one
	// month is the anchor. Empty means the calendar month.
	MonthStart string `json:"month_start"`
	// FiveHourStart pins the current 5-hour window when the proxy cannot know
	// when it really began (the ledger starts mid-window, or the account saw
	// traffic the proxy did not). It applies only until it expires; after that
	// the window is derived from the ledger again.
	FiveHourStart string `json:"five_hour_start"`
}

// fractions returns the window shares with defaults filled in. A zero or
// out-of-range value falls back to the documented default rather than
// producing a zero limit that would read as "exhausted".
func (l LimitsConfig) fractions() (five, week, warn float64) {
	five, week, warn = l.FiveHourFraction, l.WeeklyFraction, l.WarningFraction
	if five <= 0 || five > 1 {
		five = 0.2
	}
	if week <= 0 || week > 1 {
		week = 0.5
	}
	if warn <= 0 || warn >= 1 {
		warn = 0.85
	}
	return five, week, warn
}

// usageEntry is one successful, billable response.
type usageEntry struct {
	At    time.Time `json:"at"`
	USD   float64   `json:"usd"`
	Model string    `json:"model,omitempty"`
}

type usageFile struct {
	Version int          `json:"version"`
	Entries []usageEntry `json:"entries"`
}

// UsageLedger keeps a timestamped spend history so quota windows can be
// computed for rolling 5-hour/7-day periods and the current calendar month.
// An empty path keeps it in memory only.
type UsageLedger struct {
	path string

	mu      sync.Mutex
	entries []usageEntry
	dirty   bool
}

// NewUsageLedger loads path when it exists. A missing file is not an error.
func NewUsageLedger(path string) (*UsageLedger, error) {
	l := &UsageLedger{path: path}
	if path == "" {
		return l, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("usage: read %s: %w", path, err)
	}
	var f usageFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("usage: parse %s: %w", path, err)
	}
	cutoff := time.Now().Add(-usageRetention)
	for _, e := range f.Entries {
		if e.At.After(cutoff) {
			l.entries = append(l.entries, e)
		}
	}
	return l, nil
}

// Add records one charged response.
func (l *UsageLedger) Add(model string, usd float64, at time.Time) {
	if l == nil || usd == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := at.Add(-usageRetention)
	kept := l.entries[:0]
	for _, e := range l.entries {
		if e.At.After(cutoff) {
			kept = append(kept, e)
		}
	}
	l.entries = append(kept, usageEntry{At: at, USD: usd, Model: model})
	l.dirty = true
}

// UsageSnapshot is the ledger rolled up into the three quota windows, with
// the limits each of them is measured against.
type UsageSnapshot struct {
	MonthlyLimit  float64   `json:"monthly_limit"`
	FiveHourLimit float64   `json:"five_hour_limit"`
	WeeklyLimit   float64   `json:"weekly_limit"`
	FiveHourUSD   float64   `json:"five_hour_usd"`
	WeeklyUSD     float64   `json:"weekly_usd"`
	MonthlyUSD    float64   `json:"monthly_usd"`
	FiveHourReset time.Time `json:"five_hour_reset"`
	WeeklyReset   time.Time `json:"weekly_reset"`
	MonthReset    time.Time `json:"month_reset"`
	Warning       float64   `json:"warning_fraction"`
}

// Snapshot rolls the ledger up as of now.
//
// The windows follow the app's Limits panel rather than a plain sliding sum:
//
//   - 5 hours: starts on first use after the previous window ended and runs
//     for five hours, the way the panel describes it ("Starts on first use").
//   - week: fixed, starting Monday 00:00 UTC.
//   - month: fixed, anchored at MonthStart (or the calendar month).
func (l *UsageLedger) Snapshot(limits LimitsConfig, now time.Time) UsageSnapshot {
	fiveFrac, weekFrac, warn := limits.fractions()
	s := UsageSnapshot{
		MonthlyLimit:  limits.MonthlyUSD,
		FiveHourLimit: limits.MonthlyUSD * fiveFrac,
		WeeklyLimit:   limits.MonthlyUSD * weekFrac,
		Warning:       warn,
	}
	if limits.MonthlyUSD <= 0 {
		return s
	}

	weekStartAt, weekResetAt := weekWindow(now)
	monthStartAt, monthResetAt := monthWindow(now, limits.MonthStart)
	s.WeeklyReset = weekResetAt
	s.MonthReset = monthResetAt

	l.mu.Lock()
	entries := append([]usageEntry(nil), l.entries...)
	l.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })

	fiveStartAt, fiveUSD, active := fiveHourWindow(entries, now)
	if pinned, ok := parseTime(limits.FiveHourStart); ok && !now.Before(pinned) && now.Before(pinned.Add(fiveHourWindowLength)) {
		// A window pinned from the panel wins until it expires.
		fiveStartAt, active = pinned, true
		fiveUSD = 0
		for _, e := range entries {
			if !e.At.Before(pinned) && e.At.Before(pinned.Add(fiveHourWindowLength)) {
				fiveUSD += e.USD
			}
		}
	}
	s.FiveHourUSD = fiveUSD
	if active {
		s.FiveHourReset = fiveStartAt.Add(fiveHourWindowLength)
	} else {
		s.FiveHourReset = now.Add(fiveHourWindowLength)
	}

	for _, e := range entries {
		if !e.At.Before(weekStartAt) && e.At.Before(weekResetAt) {
			s.WeeklyUSD += e.USD
		}
		if !e.At.Before(monthStartAt) && e.At.Before(monthResetAt) {
			s.MonthlyUSD += e.USD
		}
	}
	return s
}

// fiveHourWindow walks the charges in order and returns the window currently
// running: a window opens at the first charge, and a charge arriving after it
// closed opens the next one.
func fiveHourWindow(entries []usageEntry, now time.Time) (start time.Time, usd float64, active bool) {
	for _, e := range entries {
		if start.IsZero() || !e.At.Before(start.Add(fiveHourWindowLength)) {
			start, usd = e.At, 0
		}
		usd += e.USD
	}
	if start.IsZero() || !now.Before(start.Add(fiveHourWindowLength)) {
		return time.Time{}, 0, false
	}
	return start, usd, true
}

// weekWindow returns the current week as the app counts it: Monday 00:00 UTC.
func weekWindow(now time.Time) (start, reset time.Time) {
	t := now.UTC()
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	start = midnight.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	return start, start.AddDate(0, 0, 7)
}

// parseTime reads an optional RFC 3339 timestamp.
func parseTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// monthWindow returns the current monthly window. With an anchor it advances
// in whole months from that instant, so a subscription that renews on the 6th
// keeps resetting on the 6th.
func monthWindow(now time.Time, anchor string) (start, reset time.Time) {
	if t, ok := parseTime(anchor); ok {
		start = t
		for !start.AddDate(0, 1, 0).After(now) {
			start = start.AddDate(0, 1, 0)
		}
		return start, start.AddDate(0, 1, 0)
	}
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return start, start.AddDate(0, 1, 0)
}

// Flush writes the ledger atomically. It is a no-op for an in-memory ledger
// and while nothing has changed since the last successful write.
func (l *UsageLedger) Flush() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	snapshot := usageFile{Version: usageFileVersion, Entries: append([]usageEntry(nil), l.entries...)}
	l.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("usage: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(l.path, data); err != nil {
		return fmt.Errorf("usage: write %s: %w", l.path, err)
	}

	l.mu.Lock()
	l.dirty = false
	l.mu.Unlock()
	return nil
}

// StartFlusher writes the ledger every interval while it has changed, and one
// last time when ctx is cancelled. The returned channel closes once that last
// write is finished.
func (l *UsageLedger) StartFlusher(ctx context.Context, every time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = l.Flush()
				return
			case <-ticker.C:
				_ = l.Flush()
			}
		}
	}()
	return done
}

// Apply writes the unified rate-limit headers Claude Code reads
// (anthropic-ratelimit-unified-*). Their contract, from the client's own
// parser: utilization is a fraction of the window (0-1, above 1 when over),
// reset is Unix epoch seconds, status is allowed | allowed_warning | rejected.
//
// The status is never "rejected": the proxy does not refuse requests it could
// still forward, and the upstream's own 429 stays authoritative. A window at
// or past its limit is reported as allowed_warning with utilization > 1, so
// the usage meter shows the overshoot without the client locking itself out.
func (s UsageSnapshot) Apply(h http.Header) {
	if s.MonthlyLimit <= 0 {
		return
	}
	five := ratio(s.FiveHourUSD, s.FiveHourLimit)
	week := ratio(s.WeeklyUSD, s.WeeklyLimit)

	writeQuotaWindow(h, "5h", five, s.FiveHourReset, s.Warning)
	writeQuotaWindow(h, "7d", week, s.WeeklyReset, s.Warning)

	claim, reset := "five_hour", s.FiveHourReset
	if week > five {
		claim, reset = "seven_day", s.WeeklyReset
	}
	h.Set("anthropic-ratelimit-unified-status", quotaStatus(max(five, week), s.Warning))
	h.Set("anthropic-ratelimit-unified-representative-claim", claim)
	h.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(reset.Unix(), 10))
}

func writeQuotaWindow(h http.Header, prefix string, util float64, reset time.Time, warn float64) {
	base := "anthropic-ratelimit-unified-" + prefix
	h.Set(base+"-utilization", strconv.FormatFloat(util, 'f', 4, 64))
	h.Set(base+"-reset", strconv.FormatInt(reset.Unix(), 10))
	h.Set(base+"-status", quotaStatus(util, warn))
	h.Set(base+"-surpassed-threshold", strconv.FormatBool(util >= warn))
}

func quotaStatus(util, warn float64) string {
	if util >= warn {
		return "allowed_warning"
	}
	return "allowed"
}

func ratio(used, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return used / limit
}
