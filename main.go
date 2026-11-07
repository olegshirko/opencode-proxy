package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Upstream: opencode.ai Anthropic-compatible inference gateway.
var upstream = "https://opencode.ai/inference/go/anthropic"

// Fallback map used only when model.txt is empty/missing.
var modelMap = map[string]string{
	"claude-opus-5":     "deepseek-v4.1-flash",
	"claude-sonnet-4-5": "deepseek-v4.1-flash",
	"claude-haiku-5-5":  "deepseek-v4.1-flash",
	"claude-fable-5":    "deepseek-v4.1-flash",
}

func main() {
	addr := flag.String("addr", "127.0.0.1:4000", "listen address")
	session := flag.String("session", "my-session-1", "x-opencode-session value")
	home, _ := os.UserHomeDir()
	modelfile := flag.String("modelfile", filepath.Join(home, "opencode-proxy", "model.txt"), "file holding the target model name (read on every request)")
	aliasfile := flag.String("aliasfile", filepath.Join(home, "opencode-proxy", "aliases.txt"), "file mapping client model IDs to upstream models (read on every request)")
	logfile := flag.String("logfile", filepath.Join(home, "opencode-proxy", "proxy.log"), "log file (appended)")
	flag.Parse()

	// Log to both the terminal and a file, so the log can be inspected later.
	if f, err := os.OpenFile(*logfile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
		defer f.Close()
	}

	client := &http.Client{} // no timeout: streaming responses

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the target model on every request, so editing model.txt
		// takes effect immediately (no rebuild/restart).
		target := ""
		if b, err := os.ReadFile(*modelfile); err == nil {
			target = strings.TrimSpace(string(b))
		}
		// aliases.txt maps the model IDs the client sends to real upstream
		// models, one "alias = target" per line; '#' starts a comment.
		aliases := map[string]string{}
		if b, err := os.ReadFile(*aliasfile); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if i := strings.Index(line, "="); i > 0 {
					aliases[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
				}
			}
		}

		url := upstream + r.URL.Path
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}

		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// ---- request log (one line per request) ----
		log.Printf(">>> %s %s (session: %s)", r.Method, r.URL.String(), r.Header.Get("X-Claude-Code-Session-Id"))

		// Rewrite the model id in any JSON body that carries one.
		if len(body) > 0 && json.Valid(body) {
			var data map[string]any
			if json.Unmarshal(body, &data) == nil {
				if m, ok := data["model"].(string); ok {
					switch {
					case aliases[m] != "" && aliases[m] != m:
						data["model"] = aliases[m]
						log.Printf("model %q -> %q (from aliases.txt)", m, aliases[m])
					case target != "" && target != m:
						data["model"] = target
						log.Printf("model %q -> %q (from model.txt)", m, target)
					case modelMap[m] != "" && modelMap[m] != m:
						data["model"] = modelMap[m]
						log.Printf("model %q -> %q (from map)", m, modelMap[m])
					}
				}
				if rewritten, err := json.Marshal(data); err == nil {
					body = rewritten
				}
			}
		}

		req, err := http.NewRequest(r.Method, url, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Route the gateway's session header off Claude Code's own session id,
		// so each conversation keeps a stable routing/cache key.
		ocSession := r.Header.Get("X-Claude-Code-Session-Id")
		if ocSession == "" {
			ocSession = *session
		}
		req.Header.Set("x-opencode-session", ocSession)
		for _, h := range []string{"Authorization", "x-api-key", "anthropic-version", "content-type", "user-agent"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		log.Printf("<<< status: %d", resp.StatusCode)

		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(resp.Body)
			log.Printf("<<< error body: %s", string(errBody))
			w.Write(errBody)
			return
		}

		// Stream the body, flushing after each chunk so SSE events reach the
		// client as they arrive.
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if rerr == io.EOF {
				return
			}
			if rerr != nil {
				return
			}
		}
	})

	log.Printf("opencode-proxy: %s -> %s (modelfile: %s)", *addr, upstream, *modelfile)
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatal(err)
	}
}
