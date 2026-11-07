package main

import (
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"time"
)

// RetryPolicy holds the pure retry arithmetic. It never sleeps; the caller
// (the proxy handler) owns the loop and the time.Sleep calls.
type RetryPolicy struct {
	MaxAttempts int
	FirstMS     int
	MaxMS       int
	BudgetMS    int
	Multiplier  float64
	Jitter      float64 // fraction, e.g. 0.2 for ±20%
	rand        *rand.Rand
}

// NewRetryPolicy builds a policy from config. Jitter is fixed at ±20%. A nil
// rnd is replaced with a source seeded from the current time.
func NewRetryPolicy(cfg RetryConfig, rnd *rand.Rand) RetryPolicy {
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return RetryPolicy{
		MaxAttempts: cfg.MaxAttempts,
		FirstMS:     cfg.FirstWaitMS,
		MaxMS:       cfg.MaxWaitMS,
		BudgetMS:    cfg.TotalBudgetMS,
		Multiplier:  cfg.Multiplier,
		Jitter:      0.2,
		rand:        rnd,
	}
}

// ShouldRetry reports whether an upstream status is worth retrying: 429 and
// 5xx are, every other status (including all remaining 4xx) is not.
func ShouldRetry(status int) bool {
	return status == 429 || (status >= 500 && status <= 599)
}

// WaitFor returns how long to wait before the given attempt. attempt is
// 1-based; values below 1 are treated as 1. The base wait is
// FirstMS * Multiplier^(attempt-1), capped at MaxMS and then jittered by
// ±Jitter. A positive retryAfter means the upstream named its own delay: it
// replaces the base, is capped at MaxMS, and is jittered the same way, so the
// result never exceeds MaxMS*(1+Jitter). WaitFor does not sleep.
func (p RetryPolicy) WaitFor(attempt int, retryAfter time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	var baseMS float64
	if retryAfter > 0 {
		baseMS = float64(retryAfter) / float64(time.Millisecond)
	} else {
		baseMS = float64(p.FirstMS) * math.Pow(p.Multiplier, float64(attempt-1))
	}
	if p.MaxMS > 0 && (baseMS > float64(p.MaxMS) || math.IsNaN(baseMS) || math.IsInf(baseMS, 1)) {
		baseMS = float64(p.MaxMS)
	}

	if p.Jitter > 0 {
		baseMS *= 1 + p.Jitter*(2*p.nextFloat()-1)
	}
	if baseMS < 0 {
		baseMS = 0
	}
	return time.Duration(baseMS * float64(time.Millisecond))
}

// nextFloat returns a value in [0,1) from the policy's source. A policy built
// as a struct literal without a source falls back to the package-level source.
func (p RetryPolicy) nextFloat() float64 {
	if p.rand != nil {
		return p.rand.Float64()
	}
	return rand.Float64()
}

// prefillHintRe matches upstream text such as "(after 30 s)".
var prefillHintRe = regexp.MustCompile(`(?i)\bafter\s+(\d+)\s*s`)

// ParsePrefillHint extracts the number of seconds from an upstream body that
// mentions a delay such as "(after 30 s)". It is for logging only: nothing
// may use its return value to decide how long to wait, because the gateway's
// prose is not a reliable delay signal. It returns 0 when the hint is absent
// or unparseable.
func ParsePrefillHint(body []byte) time.Duration {
	m := prefillHintRe.FindSubmatch(body)
	if m == nil {
		return 0
	}
	secs, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil || secs <= 0 || secs > 1<<31 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
