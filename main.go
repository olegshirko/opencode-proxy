package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// defaultConfigPath is used when -config is not given.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, "opencode-proxy", "config.json")
}

// buildRouter wires the admin control plane beside the proxy. Anything under
// /admin/ stays local; everything else is forwarded upstream.
func buildRouter(proxy *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", proxy.AdminHandler())
	// Model discovery endpoint: the app may read it instead of a hand-written
	// list. It is answered locally and never forwarded.
	mux.HandleFunc("GET /v1/models", proxy.ModelsHandler)
	// Web search for the desktop app's built-in search connector. It runs
	// through OpenCode's providers and never touches the upstream model.
	mux.HandleFunc("POST /search", SearchHandler)
	// The desktop app probes this before it will use the endpoint.
	mux.HandleFunc("HEAD /api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", proxy)
	return mux
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.json")
	addr := flag.String("addr", "", "listen address (overrides the config value)")
	flag.Parse()

	loader, err := NewConfigLoader(*configPath)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}

	modelsPath := filepath.Join(filepath.Dir(*configPath), "models.json")
	registry, err := LoadRegistry(modelsPath)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}

	logger, err := NewLogger(loader.Get().Log)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	defer logger.Close()

	cfg := loader.Get()
	dialer := &net.Dialer{Timeout: time.Duration(cfg.ConnectTimeoutSeconds) * time.Second}
	proxy := NewProxy(loader, registry, logger, &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	})

	mux := buildRouter(proxy)

	// Session cost must survive a relaunch: the desktop session outlives the
	// proxy process, so a zeroed counter would read as a bug.
	costs, err := NewCostStore(cfg.SessionCostFile)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	proxy.SetCostStore(costs)

	// The quota ledger turns accumulated allowance into the 5-hour/weekly
	// headers Claude Code shows as plan usage.
	usage, err := NewUsageLedger(cfg.UsageFile)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	proxy.SetUsageLedger(usage)
	proxy.SetScheduler(NewCellScheduler(cfg.Scheduler.MaxConcurrentCells))
	flushCtx, stopFlush := context.WithCancel(context.Background())
	defer stopFlush()
	flusherDone := costs.StartFlusher(flushCtx, 5*time.Second)
	usageDone := usage.StartFlusher(flushCtx, 5*time.Second)

	listen := cfg.Listen
	if *addr != "" {
		listen = *addr
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Pick up hand edits to config.json without a restart.
	stopReload := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopReload:
				return
			case <-ticker.C:
				if err := loader.ReloadIfChanged(); err != nil {
					log.Printf("opencode-proxy: config reload failed, keeping the last good one: %v", err)
				}
			}
		}
	}()

	// A relaunch must not cut a streaming response in half: stop accepting new
	// connections, let in-flight requests finish, then exit.
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("opencode-proxy: shutting down, draining in-flight requests")
		close(stopReload)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("opencode-proxy: drain timed out: %v", err)
		}
		close(idle)
		stopFlush()
		// Let the flusher finish its last write before this process exits:
		// exiting mid-write leaves a session-cost.json.tmp-* behind.
		<-flusherDone
		if err := costs.Flush(); err != nil {
			log.Printf("opencode-proxy: cost flush on exit failed: %v", err)
		}
		<-usageDone
		if err := usage.Flush(); err != nil {
			log.Printf("opencode-proxy: usage flush on exit failed: %v", err)
		}
	}()

	log.Printf("opencode-proxy: %s -> %s (models: %d)", listen, cfg.Upstream, len(registry.All()))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("opencode-proxy: %v", err)
	}
	<-idle
	log.Printf("opencode-proxy: stopped")
}
