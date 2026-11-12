package proxy

import (
	"encoding/json"
	"net/http"
	"opencode-proxy/internal/reqlog"
	"strings"

	"opencode-proxy/internal/catalog"
	"opencode-proxy/internal/config"
	"opencode-proxy/internal/coststore"
	"opencode-proxy/internal/models"
	"opencode-proxy/internal/picker"

	"time"
)

// adminStart is the process start time reported by /admin/status.
var adminStart = time.Now()

// AdminHandler serves the /admin/ control plane. The handler refuses
// everything when admin.enabled is false, and requires X-Admin-Token when a
// token is configured. Nothing under /admin/ is ever forwarded upstream.
func (p *Proxy) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/status", p.adminStatus)
	mux.HandleFunc("GET /admin/config", p.adminGetConfig)
	mux.HandleFunc("PATCH /admin/config", p.adminPatchConfig)
	mux.HandleFunc("GET /admin/log/capture", p.adminGetCapture)
	mux.HandleFunc("PUT /admin/log/capture", p.adminPutCapture)
	mux.HandleFunc("GET /admin/models", p.adminModels)
	mux.HandleFunc("PUT /admin/models/aliases", p.adminPutAliases)
	mux.HandleFunc("POST /admin/reload", p.adminReload)
	mux.HandleFunc("POST /admin/shutdown", p.adminShutdown)
	mux.HandleFunc("GET /admin/session-cost", p.adminSessionCost)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := p.cfg.Get()
		if !cfg.Admin.Enabled {
			http.NotFound(w, r)
			return
		}
		if cfg.Admin.Token != "" && r.Header.Get("X-Admin-Token") != cfg.Admin.Token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// ModelsHandler serves the Anthropic model-list shape from the gateway's own
// catalog, so the picker follows the subscription instead of a snapshot file.
// The registry stays as the fallback for when the catalog cannot be read, and
// it also says which models are known to be unavailable.
func (p *Proxy) ModelsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := p.cfg.Get()
	type entry struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}

	out := make([]entry, 0, 32)
	live, err := p.catalog.Get(r.Context(), catalog.URLFor(cfg.Models.CatalogURL, cfg.Upstream), p.client, r.Header.Get("Authorization"))
	switch {
	case err == nil && len(live) > 0:
		// The gateway's list is authoritative: it names the models this
		// subscription has. Availability is a per-request condition that
		// changes (country, opt-ins, transient outages), so nothing is hidden
		// here — a model that cannot run reports its own error when used.
		for _, m := range live {
			out = append(out, entry{Type: "model", ID: m.Picker, DisplayName: m.Name})
		}
	default:
		// The gateway list is unreachable: serve the last snapshot rather than
		// an empty picker.
		for _, m := range p.registry().All() {
			if !m.Available {
				continue
			}
			id := m.Picker
			if id == "" {
				id = m.ID
			}
			lower := strings.ToLower(id)
			if !strings.HasPrefix(lower, "claude") && !strings.Contains(lower, "anthropic") {
				continue
			}
			out = append(out, entry{Type: "model", ID: id, DisplayName: m.Label})
		}
	}

	// Routed models are not in the gateway catalog at all: they live on
	// another endpoint and are declared in the config, so they are added here
	// rather than being dropped from the picker.
	seen := make(map[string]bool, len(out))
	for _, m := range out {
		seen[strings.ToLower(m.ID)] = true
	}
	for _, route := range cfg.Models.Routes {
		if route.ID == "" {
			continue
		}
		id := route.Picker
		if id == "" {
			id = picker.Name(route.ID, route.Label)
		}
		if seen[strings.ToLower(id)] {
			continue
		}
		label := route.Label
		if label == "" {
			label = route.ID
		}
		out = append(out, entry{Type: "model", ID: id, DisplayName: label})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":     out,
		"has_more": false,
	})
}

func (p *Proxy) adminStatus(w http.ResponseWriter, r *http.Request) {
	cfg := p.cfg.Get()
	st := p.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"upstream":         cfg.Upstream,
		"listen":           cfg.Listen,
		"started_at":       adminStart.UTC().Format(time.RFC3339),
		"uptime_seconds":   int(time.Since(adminStart).Seconds()),
		"models":           len(p.registry().All()),
		"requests":         st.Requests,
		"retries":          st.Retries,
		"upstream_429":     st.Upstream429,
		"in_flight":        st.InFlight,
		"queued":           st.Queued,
		"cells_in_use":     st.CellsInUse,
		"queue_wait_ms":    st.QueueWaitMS,
		"keepalives":       st.Keepalives,
		"max_cells":        cfg.Scheduler.MaxConcurrentCells,
		"cell_limit":       st.CellLimit,
		"prefill_refusals": st.PrefillRefusals,
		"quota":            p.Quota(cfg.Limits, time.Now()),
		"qoder":            p.qoderManagerFor(cfg).Status(),
	})
}

func (p *Proxy) adminGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, redactedConfig(p.cfg.Get()))
}

// capturePatch uses pointers so an omitted key keeps its current value
// instead of silently clearing it.
type capturePatch struct {
	RequestHeaders  *bool `json:"request_headers"`
	RequestBody     *bool `json:"request_body"`
	ResponseHeaders *bool `json:"response_headers"`
	ResponseBody    *bool `json:"response_body"`
}

func applyCapturePatch(c *reqlog.Capture, patch capturePatch) {
	if patch.RequestHeaders != nil {
		c.RequestHeaders = *patch.RequestHeaders
	}
	if patch.RequestBody != nil {
		c.RequestBody = *patch.RequestBody
	}
	if patch.ResponseHeaders != nil {
		c.ResponseHeaders = *patch.ResponseHeaders
	}
	if patch.ResponseBody != nil {
		c.ResponseBody = *patch.ResponseBody
	}
}

type logPatch struct {
	Capture *capturePatch `json:"capture"`
	Level   *string       `json:"level"`
	MaxMB   *int          `json:"max_mb"`
	Keep    *int          `json:"keep"`
}

type modelsPatch struct {
	Default    *string            `json:"default"`
	Background *string            `json:"background"`
	Aliases    *map[string]string `json:"aliases"`
}

type retryPatch struct {
	MaxAttempts   *int     `json:"max_attempts"`
	FirstWaitMS   *int     `json:"first_wait_ms"`
	Multiplier    *float64 `json:"multiplier"`
	MaxWaitMS     *int     `json:"max_wait_ms"`
	TotalBudgetMS *int     `json:"total_budget_ms"`
}

type adminPatch struct {
	Token *string `json:"token"`
}

type configPatch struct {
	Log    *logPatch    `json:"log"`
	Models *modelsPatch `json:"models"`
	Retry  *retryPatch  `json:"retry"`
	Admin  *adminPatch  `json:"admin"`
}

func (p *Proxy) adminPatchConfig(w http.ResponseWriter, r *http.Request) {
	var patch configPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	err := p.cfg.Update(func(c *config.Config) error {
		if patch.Log != nil {
			if patch.Log.Capture != nil {
				applyCapturePatch(&c.Log.Capture, *patch.Log.Capture)
			}
			if patch.Log.Level != nil {
				c.Log.Level = *patch.Log.Level
			}
			if patch.Log.MaxMB != nil {
				c.Log.MaxMB = *patch.Log.MaxMB
			}
			if patch.Log.Keep != nil {
				c.Log.Keep = *patch.Log.Keep
			}
		}
		if patch.Models != nil {
			if patch.Models.Default != nil {
				c.Models.Default = *patch.Models.Default
			}
			if patch.Models.Background != nil {
				c.Models.Background = *patch.Models.Background
			}
			if patch.Models.Aliases != nil {
				c.Models.Aliases = copyAliases(*patch.Models.Aliases)
			}
		}
		if patch.Retry != nil {
			if patch.Retry.MaxAttempts != nil {
				c.Retry.MaxAttempts = *patch.Retry.MaxAttempts
			}
			if patch.Retry.FirstWaitMS != nil {
				c.Retry.FirstWaitMS = *patch.Retry.FirstWaitMS
			}
			if patch.Retry.Multiplier != nil {
				c.Retry.Multiplier = *patch.Retry.Multiplier
			}
			if patch.Retry.MaxWaitMS != nil {
				c.Retry.MaxWaitMS = *patch.Retry.MaxWaitMS
			}
			if patch.Retry.TotalBudgetMS != nil {
				c.Retry.TotalBudgetMS = *patch.Retry.TotalBudgetMS
			}
		}
		if patch.Admin != nil && patch.Admin.Token != nil {
			c.Admin.Token = *patch.Admin.Token
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, redactedConfig(p.cfg.Get()))
}

func (p *Proxy) adminGetCapture(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.cfg.Get().Log.Capture)
}

func (p *Proxy) adminPutCapture(w http.ResponseWriter, r *http.Request) {
	var patch capturePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	err := p.cfg.Update(func(c *config.Config) error {
		applyCapturePatch(&c.Log.Capture, patch)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Get().Log.Capture)
}

type adminModel struct {
	models.Model
	ResolvesTo string `json:"resolves_to"`
	Reason     string `json:"reason"`
}

func (p *Proxy) adminModels(w http.ResponseWriter, r *http.Request) {
	cfg := p.cfg.Get()
	all := p.registry().All()
	out := make([]adminModel, 0, len(all))
	for _, m := range all {
		key := m.Picker
		if key == "" {
			key = m.ID
		}
		target, reason := p.registry().Resolve(key, cfg.Models)
		out = append(out, adminModel{Model: m, ResolvesTo: target, Reason: reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":     out,
		"aliases":    cfg.Models.Aliases,
		"default":    cfg.Models.Default,
		"background": cfg.Models.Background,
	})
}

func (p *Proxy) adminPutAliases(w http.ResponseWriter, r *http.Request) {
	var aliases map[string]string
	if err := json.NewDecoder(r.Body).Decode(&aliases); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if aliases == nil {
		aliases = map[string]string{}
	}
	if err := p.cfg.Update(func(c *config.Config) error {
		c.Models.Aliases = copyAliases(aliases)
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Get().Models.Aliases)
}

func (p *Proxy) adminReload(w http.ResponseWriter, r *http.Request) {
	if err := p.cfg.ReloadIfChanged(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, redactedConfig(p.cfg.Get()))
}

// adminShutdown answers before the drain begins: srv.Shutdown waits for this
// very request to finish, so the hook must only signal, never block.
func (p *Proxy) adminShutdown(w http.ResponseWriter, r *http.Request) {
	if p.shutdown == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "shutdown hook not configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	p.shutdown()
}

type sessionCostResponse struct {
	Session  string                         `json:"session"`
	USD      float64                        `json:"usd"`
	ListUSD  float64                        `json:"list_usd"`
	Requests int                            `json:"requests"`
	FirstAt  string                         `json:"first_at"`
	LastAt   string                         `json:"last_at"`
	ByModel  map[string]coststore.ModelCost `json:"by_model"`
}

func (p *Proxy) adminSessionCost(w http.ResponseWriter, r *http.Request) {
	sc := p.SessionCost(r.URL.Query().Get("session"))
	if sc.ByModel == nil {
		sc.ByModel = map[string]coststore.ModelCost{}
	}
	writeJSON(w, http.StatusOK, sessionCostResponse{
		Session:  sc.Session,
		USD:      sc.USD,
		ListUSD:  sc.ListUSD,
		Requests: sc.Requests,
		FirstAt:  formatOptionalTime(sc.FirstAt),
		LastAt:   formatOptionalTime(sc.LastAt),
		ByModel:  sc.ByModel,
	})
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// redactedConfig returns a copy safe to send over HTTP: the admin token is
// replaced rather than omitted, so the caller can see that one is set.
func redactedConfig(c *config.Config) *config.Config {
	out := *c
	out.Models.Aliases = copyAliases(c.Models.Aliases)
	if out.Admin.Token != "" {
		out.Admin.Token = "***"
	}
	return &out
}

func copyAliases(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
