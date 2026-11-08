// Command oc-search-mcp is a minimal MCP server that gives Claude Code a web
// search tool backed by the local OpenCode server's own web-search providers
// (exa, tavily, firecrawl, parallel, tinyfish, ...).
//
// Claude Code's built-in WebSearch is Anthropic's server-side tool and is not
// offered behind a third-party gateway, so this is the documented way to add
// search: an MCP server that exposes a search tool.
// https://code.claude.com/docs/en/tools-reference
//
// The search itself is delegated to `opencode api POST /api/websearch`, which
// uses the CLI's own connection to its background server, so the server's
// credentials never leave the machine. The same search also backs the proxy's
// /search endpoint (see internal/websearch).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"opencode-proxy/internal/websearch"
)

const (
	serverName    = "oc-search"
	serverVersion = "1.0.0"
	maxSnippet    = 400
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type callParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
		Limit    int    `json:"limit"`
	} `json:"arguments"`
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false)

	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		// Notifications carry no id and expect no answer.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		resp := handle(req)
		if err := out.Encode(resp); err != nil {
			return
		}
	}
}

func handle(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p initializeParams
		_ = json.Unmarshal(req.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = "2024-11-05"
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": []any{searchTool()}}
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		if p.Name != "search" {
			resp.Error = &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
			return resp
		}
		text, isErr := search(p.Arguments.Query, p.Arguments.Provider, p.Arguments.Limit)
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
			"isError": isErr,
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func searchTool() map[string]any {
	return map[string]any{
		"name": "search",
		"description": "Search the web and return titles, URLs and short snippets. " +
			"The snippets are excerpts, not full pages: to read a result, fetch its URL with WebFetch. " +
			"Use this instead of guessing at current information.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query.",
				},
				"provider": map[string]any{
					"type":        "string",
					"description": "Optional provider id (exa, tavily, firecrawl, parallel, tinyfish). Defaults to the configured one.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of results to return (1-10).",
					"minimum":     1,
					"maximum":     websearch.MaxResults,
				},
			},
			"required": []string{"query"},
		},
	}
}

// search runs one query through the OpenCode server and formats the results
// for the model. It returns isError instead of an MCP error so the model sees
// what went wrong and can react.
func search(query, provider string, limit int) (string, bool) {
	query = strings.TrimSpace(query)
	res, err := websearch.Search(context.Background(), query, provider, limit)
	if err != nil {
		return "search failed: " + err.Error(), true
	}

	results := res.Results
	if len(results) == 0 {
		return "No results for: " + query, false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q via %s:\n", len(results), query, res.ProviderID)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if snippet := trimText(r.Snippet, maxSnippet); snippet != "" {
			fmt.Fprintf(&b, "   %s\n", snippet)
		}
	}
	return strings.TrimRight(b.String(), "\n"), false
}

// trimText shortens s to at most n runes without cutting one in half.
func trimText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	return strings.TrimRight(string(r), " ") + "…"
}
