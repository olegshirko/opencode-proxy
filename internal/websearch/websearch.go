// Package websearch runs web searches through the local OpenCode server's own
// providers (exa, tavily, firecrawl, parallel, tinyfish, ...).
//
// The search is delegated to `opencode api POST /api/websearch`, which uses the
// CLI's connection to its background server, so the providers' credentials
// never leave this machine. Two callers share it: the oc-search MCP server and
// the proxy's /search endpoint, which backs the desktop app's built-in web
// search.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// DefaultProvider is used when neither the caller nor the environment names
	// one.
	DefaultProvider = "tinyfish"
	// MaxResults caps how many hits one query may return.
	MaxResults = 10
	// Timeout bounds one search, including the OpenCode CLI round trip.
	Timeout = 45 * time.Second
)

// Result is one search hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Response is one completed search.
type Response struct {
	ProviderID string
	Results    []Result
}

// ProviderError reports a failure the search provider itself named, so the
// caller can repeat its message instead of inventing one.
type ProviderError struct {
	Message string
}

func (e *ProviderError) Error() string { return e.Message }

type apiResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type apiResponse struct {
	Data struct {
		ProviderID string      `json:"providerID"`
		Results    []apiResult `json:"results"`
	} `json:"data"`
	Tag     string `json:"_tag"`
	Message string `json:"message"`
	Service string `json:"service"`
}

// Search runs one query and returns at most limit hits. An empty provider falls
// back to OC_WEBSEARCH_PROVIDER, then to DefaultProvider.
func Search(ctx context.Context, query, provider string, limit int) (Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Response{}, errors.New("the query is empty")
	}
	if provider == "" {
		provider = os.Getenv("OC_WEBSEARCH_PROVIDER")
	}
	if provider == "" {
		provider = DefaultProvider
	}
	if limit <= 0 || limit > MaxResults {
		limit = MaxResults
	}

	body, err := json.Marshal(map[string]any{"query": query, "providerID": provider})
	if err != nil {
		return Response{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, CLI(), "api", "POST", "/api/websearch", "-d", string(body))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return Response{}, errors.New(msg)
	}

	var res apiResponse
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return Response{}, errors.New("unexpected answer from the OpenCode server: " + err.Error())
	}
	if res.Tag != "" {
		msg := res.Message
		if res.Service != "" {
			msg += " (" + res.Service + ")"
		}
		if msg == "" {
			msg = res.Tag
		}
		return Response{}, &ProviderError{Message: msg +
			"\n\nNo provider is configured for this one. OpenCode accepts exa, tavily, firecrawl, parallel and tinyfish; " +
			"set websearch.provider and its key in the OpenCode config, or pass a working provider."}
	}

	results := res.Data.Results
	if len(results) > limit {
		results = results[:limit]
	}
	out := Response{ProviderID: res.Data.ProviderID, Results: make([]Result, 0, len(results))}
	for _, r := range results {
		out.Results = append(out.Results, Result{Title: strings.TrimSpace(r.Title), URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// CLI finds the OpenCode command. Callers inherit the user's environment, but
// PATH is not guaranteed, so the usual install locations are tried before
// giving up.
func CLI() string {
	if p := os.Getenv("OC_CLI"); p != "" {
		return p
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		home + "/.opencode/bin/opencode",
		"/opt/homebrew/bin/opencode",
		"/usr/local/bin/opencode",
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "opencode"
}
