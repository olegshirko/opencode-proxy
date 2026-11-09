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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"opencode-proxy/internal/picker"
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

// FetchCatalog reads the gateway's model catalog.
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

// PickerName turns a native id into the id the desktop app shows. The rules
// live in internal/picker so the proxy names live catalog entries the same way.
func PickerName(id, name string) string {
	return picker.Name(id, name)
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
	// max_tokens must clear each model's minimum: some answer
	// integer_below_min_value at 1 and would be recorded as unavailable.
	body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, m.ID)
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

// options are the command line flags, kept out of main so the work can be
// tested without touching the real network or real flags.
type options struct {
	out        string
	probe      bool
	tokenFile  string
	catalogURL string
	probeURL   string
	settings   bool
	proxyURL   string
}

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

// runMain is the command itself, separated from main so it can be tested.
func runMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("genmodels", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "models.json", "output file")
	probe := fs.Bool("probe", false, "probe every model and record availability")
	settings := fs.Bool("settings", false, "print the Claude Code settings block (modelPicker) and exit")
	proxyURL := fs.String("proxy-url", "http://127.0.0.1:4000", "proxy base URL for the settings block")
	tokenFile := fs.String("token-file", "", "JSON file holding an Anthropic token (e.g. ~/.claude/settings.json)")
	catalogURL := fs.String("catalog", defaultCatalogURL, "catalog endpoint")
	probeURL := fs.String("probe-url", defaultProbeURL, "base URL for the availability probe")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opts := options{
		out:        *out,
		probe:      *probe,
		tokenFile:  *tokenFile,
		catalogURL: *catalogURL,
		probeURL:   *probeURL,
		settings:   *settings,
		proxyURL:   *proxyURL,
	}
	if err := run(ctx, opts, &http.Client{Timeout: 90 * time.Second}, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "genmodels:", err)
		return 1
	}
	return 0
}

// run fetches the catalog, optionally probes every model, and writes the
// models.json the proxy and the app profile generator read.
func run(ctx context.Context, opts options, hc *http.Client, stdout, stderr io.Writer) error {
	items, err := FetchCatalog(ctx, opts.catalogURL, hc)
	if err != nil {
		return err
	}

	models := make([]model, 0, len(items))
	for _, cm := range items {
		models = append(models, toModel(cm))
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Label < models[j].Label })

	if opts.probe {
		token := os.Getenv("OPENCODE_TOKEN")
		if token == "" && opts.tokenFile != "" {
			token = readToken(opts.tokenFile)
		}
		if token == "" {
			return fmt.Errorf("-probe needs OPENCODE_TOKEN or -token-file")
		}
		for i := range models {
			models[i].Available = Probe(ctx, opts.probeURL, token, models[i], hc)
			fmt.Fprintf(stderr, "probe %-28s %v\n", models[i].ID, models[i].Available)
		}
	} else {
		carryAvailability(opts.out, models)
	}

	if opts.settings {
		return writeSettings(stdout, opts.proxyURL, models)
	}

	data, err := json.MarshalIndent(modelFile{Models: models}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(opts.out, data, 0o644); err != nil {
		return err
	}

	available := 0
	for _, m := range models {
		if m.Available {
			available++
		}
	}
	fmt.Fprintf(stdout, "wrote %s: %d models, %d available\n", opts.out, len(models), available)
	return nil
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
		Env   map[string]string `json:"env"`
		Token string            `json:"ANTHROPIC_AUTH_TOKEN"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	if tok := doc.Env["ANTHROPIC_AUTH_TOKEN"]; tok != "" {
		return tok
	}
	return doc.Token
}

// carryAvailability copies each model's `available` flag from the previous
// models.json. The flag comes from the regional probe; a run without -probe
// cannot re-check it, so the last probed answer stands. Models the file does
// not know yet start available: the picker should show a brand-new catalog
// entry until a probe says otherwise.
func carryAvailability(out string, models []model) {
	data, err := os.ReadFile(out)
	if err != nil {
		return
	}
	var prev modelFile
	if json.Unmarshal(data, &prev) != nil {
		return
	}
	known := make(map[string]bool, len(prev.Models))
	for _, m := range prev.Models {
		known[m.ID] = m.Available
	}
	for i := range models {
		if av, ok := known[models[i].ID]; ok {
			models[i].Available = av
		}
	}
}

// writeSettings prints the Claude Code settings block: the env pointing at the
// proxy and a modelPicker with the models the region actually serves. Only
// available models are offered — a RegionError on a blocked pick is
// readable, but silently missing models are not.
func writeSettings(w io.Writer, proxyURL string, models []model) error {
	block := struct {
		Env         map[string]string `json:"env"`
		ModelPicker struct {
			ReplaceBuiltInOptions bool             `json:"replaceBuiltInOptions"`
			Options               []pickerSettings `json:"options"`
		} `json:"modelPicker"`
	}{
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":   proxyURL,
			"ANTHROPIC_AUTH_TOKEN": "<your OpenCode Go token>",
		},
	}
	block.ModelPicker.ReplaceBuiltInOptions = true
	for _, m := range models {
		if !m.Available {
			continue
		}
		block.ModelPicker.Options = append(block.ModelPicker.Options, pickerSettings{
			Model:     m.Picker,
			Label:     m.Label,
			BehavesAs: m.BehavesAs,
		})
	}
	data, err := json.MarshalIndent(block, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

type pickerSettings struct {
	Model     string `json:"model"`
	Label     string `json:"label"`
	BehavesAs string `json:"behavesAs,omitempty"`
}
