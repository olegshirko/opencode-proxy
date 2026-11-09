// Строка статуса для Code-вкладки: показывает реальную стоимость текущей
// сессии из /admin/session-cost прокси, а не оценку Claude Code.
//
// Контракт приложения: на stdin одна JSON-строка
//
//	{"session_id": "local_<uuid>", "model": {"id": "...", "display_name": "..."}}
//
// stdout: первая непустая строка до 200 символов. При любой ошибке печатаем
// пустую строку — приложение просто не показывает строку статуса.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const proxyBase = "http://127.0.0.1:4000"

type input struct {
	SessionID string `json:"session_id"`
	Model     struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	// RateLimits arrives once the proxy reports the Go allowance through the
	// anthropic-ratelimit-unified-* response headers.
	RateLimits struct {
		FiveHour *struct {
			UsedPercentage float64 `json:"used_percentage"`
		} `json:"five_hour"`
		SevenDay *struct {
			UsedPercentage float64 `json:"used_percentage"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

type sessionCost struct {
	Session  string  `json:"session"`
	USD      float64 `json:"usd"`
	ListUSD  float64 `json:"list_usd"`
	Requests int     `json:"requests"`
	ByModel  map[string]struct {
		USD float64 `json:"usd"`
	} `json:"by_model"`
}

// resolveSessionID maps the statusline input id to the id the proxy knows.
// The desktop app sends local_… ids that need the macOS session-directory
// lookup; the Claude Code CLI sends its own conversation id directly, which
// is exactly the X-Claude-Code-Session-Id the proxy logs.
func resolveSessionID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("unexpected session id")
	}
	if strings.HasPrefix(id, "local_") {
		return cliSessionID(id)
	}
	return id, nil
}

// cliSessionID находит CLI-сессию (её же видит прокси как X-Claude-Code-Session-Id)
// по локальному id сессии приложения.
func cliSessionID(localID string) (string, error) {
	if localID == "" || !strings.HasPrefix(localID, "local_") {
		return "", fmt.Errorf("unexpected session id")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	pattern := filepath.Join(home, "Library", "Application Support",
		"Claude-3p", "claude-code-sessions", "*", "*", localID+".json")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("session file not found")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return "", err
	}
	var meta struct {
		CliSessionID string `json:"cliSessionId"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.CliSessionID == "" {
		return "", fmt.Errorf("no cliSessionId")
	}
	return meta.CliSessionID, nil
}

func fetchCost(cliID string) (*sessionCost, error) {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(proxyBase + "/admin/session-cost?session=" + cliID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var sc sessionCost
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// quota is the spend the proxy has accounted for in the current windows.
type quota struct {
	FiveHourUSD   float64 `json:"five_hour_usd"`
	FiveHourLimit float64 `json:"five_hour_limit"`
	WeeklyUSD     float64 `json:"weekly_usd"`
	WeeklyLimit   float64 `json:"weekly_limit"`
	MonthlyUSD    float64 `json:"monthly_usd"`
	MonthlyLimit  float64 `json:"monthly_limit"`
}

// fetchQuota reads the same numbers the proxy turns into rate-limit headers.
// The client does not surface those headers here (it reserves five_hour and
// seven_day for claude.ai subscriptions), so the line computes them itself.
func fetchQuota() (*quota, error) {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(proxyBase + "/admin/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var st struct {
		Quota quota `json:"quota"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st.Quota, nil
}

// percent matches the app's Limits panel, which rounds to whole percents.
func percent(used, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return math.Round(100 * used / limit)
}

func formatUSD(usd float64) string {
	if usd > 0 && usd < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.4f", usd)
}

func main() {
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 16*1024))
	out := ""
	var in input
	if err := json.Unmarshal(raw, &in); err == nil {
		parts := make([]string, 0, 6)
		sessionCost := 0.0
		sessionReqs := 0
		if cliID, err := resolveSessionID(in.SessionID); err == nil {
			if sc, err := fetchCost(cliID); err == nil {
				sessionCost, sessionReqs = sc.USD, sc.Requests
			}
		}
		if sessionReqs > 0 {
			parts = append(parts, formatUSD(sessionCost), fmt.Sprintf("%d req", sessionReqs))
		} else {
			parts = append(parts, "no requests")
		}
		// The client leaves these windows empty for a gateway session, so the
		// percentages come from the proxy's own accounting instead.
		if in.RateLimits.FiveHour == nil {
			if q, err := fetchQuota(); err == nil && q.FiveHourLimit > 0 {
				// The month spend is the base the percentages measure, so it
				// goes next to them: the session's own cost sits beside it.
				parts = append(parts, fmt.Sprintf("month $%.2f", q.MonthlyUSD))
				parts = append(parts, fmt.Sprintf("W %.0f%%", percent(q.WeeklyUSD, q.WeeklyLimit)))
				parts = append(parts, fmt.Sprintf("D %.0f%%", percent(q.FiveHourUSD, q.FiveHourLimit)))
				parts = append(parts, fmt.Sprintf("M %.0f%%", percent(q.MonthlyUSD, q.MonthlyLimit)))
			}
		} else {
			parts = append(parts, fmt.Sprintf("D %.0f%%", in.RateLimits.FiveHour.UsedPercentage))
			if in.RateLimits.SevenDay != nil {
				parts = append(parts, fmt.Sprintf("W %.0f%%", in.RateLimits.SevenDay.UsedPercentage))
			}
		}
		model := in.Model.DisplayName
		if model == "" {
			model = in.Model.ID
		}
		if model = strings.TrimSpace(model); model != "" {
			parts = append(parts, model)
		}
		out = strings.Join(parts, " · ")
	}
	fmt.Println(out)
}
