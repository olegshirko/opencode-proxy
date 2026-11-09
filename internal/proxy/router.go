package proxy

import "net/http"

// Router wires the admin control plane beside the proxy. Anything under
// /admin/ stays local; everything else is forwarded upstream.
func Router(p *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", p.AdminHandler())
	// Model discovery endpoint: the app may read it instead of a hand-written
	// list. It is answered locally and never forwarded.
	mux.HandleFunc("GET /v1/models", p.ModelsHandler)
	// Web search for the desktop app's built-in search connector. It runs
	// through OpenCode's providers and never touches the upstream model.
	mux.HandleFunc("POST /search", SearchHandler)
	// The desktop app probes this before it will use the endpoint.
	mux.HandleFunc("HEAD /api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", p)
	return mux
}
