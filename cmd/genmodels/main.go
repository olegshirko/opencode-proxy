// Command genmodels builds models.json from the OpenCode Go catalog.
//
//	go run ./cmd/genmodels -out models.json
//	go run ./cmd/genmodels -out models.json -probe -token-file ~/.claude/settings.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultCatalogURL = "https://opencode.ai/inference/go/v1/models"
	defaultProbeURL   = "https://opencode.ai/inference/go/anthropic"
)

// --- catalog types -------------------------------------------------------

type catalog struct {
	Data []catalogModel `json:"data"`
}

type catalogModel struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Capabilities struct {
		Reasoning struct {
			Effort []string `json:"effort"`
		} `json:"reasoning"`
	} `json:"capabilities"`
	Limit struct {
		Context int `json:"context"`
	} `json:"limit"`
	Cost struct {
		Text struct {
			Input struct {
				Price float64 `json:"price"`
				Cache struct {
					Read float64 `json:"read"`
				} `json:"cache"`
			} `json:"input"`
			Output struct {
				Price float64 `json:"price"`
			} `json:"output"`
		} `json:"text"`
	} `json:"cost"`
}

// --- output types (must match models.json) -------------------------------

type pricing struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

type model struct {
	ID         string  `json:"id"`
	Picker     string  `json:"picker"`
	Label      string  `json:"label"`
	BehavesAs  string  `json:"behaves_as"`
	MaxEffort  string  `json:"max_effort"`
	Supports1M bool    `json:"supports_1m"`
	Available  bool    `json:"available"`
	Context    int     `json:"context"`
	Pricing    pricing `json:"pricing"`
}

type modelFile struct {
	Models []model `json:"models"`
}

// effortOrder is the ladder used to pick the highest supported level.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// pickerOverrides renames picker ids where the catalog spelling is too long.
// The value goes after the "claude-" prefix, so "deepseek-v4-pro" becomes
// "claude-DPV4-Pro". Lowercased, it must be resolvable through the picker
// table in models.json — which the generator writes — so any spelling works.
var pickerOverrides = map[string]string{
	"deepseek-v4-pro": "DPV4-Pro",
}

// FetchCatalog reads the Go model catalog.
func FetchCatalog(ctx context.Context, url string, hc *http.Client) ([]catalogModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: HTTP %d", resp.StatusCode)
	}
	var c catalog
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return nil, fmt.Errorf("catalog: decode: %w", err)
	}
	return c.Data, nil
}

// PickerName turns a native id into the id the desktop app shows. It prefers
// the catalog's display name with spaces as dashes, because that keeps
// acronyms ("GLM") readable, and falls back to the id when the display name
// does not spell the id (as with "Claude Haiku 5.5" vs claude-haiku-5-5).
// Either way only case and separators differ, so the proxy's prefix rule maps
// it straight back. Ids that already start with "claude-" get the prefix
// twice on purpose: otherwise the proxy's background rule would swallow them.
func PickerName(id, name string) string {
	if strings.HasPrefix(id, "claude-") {
		return "claude-" + id
	}
	if short, ok := pickerOverrides[id]; ok {
		return "claude-" + short
	}
	if candidate := strings.ReplaceAll(strings.TrimSpace(name), " ", "-"); candidate != "" &&
		strings.ToLower(candidate) == id {
		return "claude-" + candidate
	}
	return "claude-" + id
}

// MaxEffort returns the highest effort level the model supports, or "low"
// when it advertises none. The first element of the ladder is "none", which
// means the model does not do reasoning, so it never wins.
func MaxEffort(levels []string) string {
	best := ""
	bestRank := -1
	for _, l := range levels {
		rank := -1
		for i, known := range effortOrder {
			if known == l {
				rank = i
				break
			}
		}
		if rank > bestRank {
			best, bestRank = l, rank
		}
	}
	if best == "" || best == "none" || best == "minimal" {
		return "low"
	}
	return best
}

// BehavesAs picks the Claude model whose context window matches, so the app
// reports the right limits for a non-Claude id.
func BehavesAs(contextWindow int) string {
	if contextWindow >= 1_000_000 {
		return "claude-sonnet-5-5"
	}
	return "claude-sonnet-5"
}

func toModel(cm catalogModel) model {
	return model{
		ID:         cm.ID,
		Picker:     PickerName(cm.ID, cm.Name),
		Label:      cm.Name,
		BehavesAs:  BehavesAs(cm.Limit.Context),
		MaxEffort:  MaxEffort(cm.Capabilities.Reasoning.Effort),
		Supports1M: cm.Limit.Context >= 1_000_000,
		Available:  true,
		Context:    cm.Limit.Context,
		Pricing: pricing{
			Input:     cm.Cost.Text.Input.Price,
			Output:    cm.Cost.Text.Output.Price,
			CacheRead: cm.Cost.Text.Input.Cache.Read,
		},
	}
}

// Probe sends a one-token request to see whether the model is reachable from
// this region. Any non-2xx answer counts as unavailable.
func Probe(ctx context.Context, baseURL, token string, m model, hc *http.Client) bool {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, m.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", token)
	req.Header.Set("x-opencode-session", "genmodels-probe")
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func main() {
	out := flag.String("out", "models.json", "output file")
	probe := flag.Bool("probe", false, "probe every model and record availability")
	tokenFile := flag.String("token-file", "", "JSON file holding an Anthropic token (e.g. ~/.claude/settings.json)")
	catalogURL := flag.String("catalog", defaultCatalogURL, "catalog endpoint")
	probeURL := flag.String("probe-url", defaultProbeURL, "base URL for the availability probe")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	hc := &http.Client{Timeout: 90 * time.Second}

	items, err := FetchCatalog(ctx, *catalogURL, hc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genmodels:", err)
		os.Exit(1)
	}

	models := make([]model, 0, len(items))
	for _, cm := range items {
		models = append(models, toModel(cm))
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Label < models[j].Label })

	if *probe {
		token := os.Getenv("OPENCODE_TOKEN")
		if token == "" && *tokenFile != "" {
			token = readToken(*tokenFile)
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "genmodels: -probe needs OPENCODE_TOKEN or -token-file")
			os.Exit(1)
		}
		for i := range models {
			models[i].Available = Probe(ctx, *probeURL, token, models[i], hc)
			fmt.Fprintf(os.Stderr, "probe %-28s %v\n", models[i].ID, models[i].Available)
		}
	}

	data, err := json.MarshalIndent(modelFile{Models: models}, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "genmodels:", err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genmodels:", err)
		os.Exit(1)
	}
	available := 0
	for _, m := range models {
		if m.Available {
			available++
		}
	}
	fmt.Printf("wrote %s: %d models, %d available\n", *out, len(models), available)
}

// readToken pulls ANTHROPIC_AUTH_TOKEN out of a Claude settings file. It
// accepts either {"env":{"ANTHROPIC_AUTH_TOKEN":...}} or a top-level key.
func readToken(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	return doc.Env["ANTHROPIC_AUTH_TOKEN"]
}
