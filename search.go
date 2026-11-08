package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"opencode-proxy/internal/websearch"
)

// searchRequest accepts both spellings: the desktop app's built-in web-search
// connector posts {"q": ...}, and a person's curl often sends {"query": ...}.
type searchRequest struct {
	Q        string `json:"q"`
	Query    string `json:"query"`
	Provider string `json:"provider"`
	Limit    int    `json:"limit"`
}

type searchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// SearchHandler answers POST /search with {"results": [{title, url, snippet}]},
// the shape the desktop app's custom web-search provider expects. The search
// itself runs through OpenCode, so no provider keys live in the app config.
func SearchHandler(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	query := strings.TrimSpace(req.Q)
	if query == "" {
		query = strings.TrimSpace(req.Query)
	}
	if query == "" {
		writeJSONError(w, http.StatusBadRequest, "q is required")
		return
	}

	res, err := websearch.Search(r.Context(), query, req.Provider, req.Limit)
	if err != nil {
		status := http.StatusBadGateway
		var providerErr *websearch.ProviderError
		if errors.As(err, &providerErr) {
			status = http.StatusServiceUnavailable
		}
		writeJSONError(w, status, err.Error())
		return
	}

	hits := make([]searchHit, 0, len(res.Results))
	for _, r := range res.Results {
		hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": res.ProviderID,
		"results":  hits,
	})
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
