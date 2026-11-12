package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"opencode-proxy/internal/config"
	"opencode-proxy/internal/coststore"
	"opencode-proxy/internal/models"
	"opencode-proxy/internal/proxy"
	"opencode-proxy/internal/quota"
	"opencode-proxy/internal/reqlog"
	"opencode-proxy/internal/scheduler"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultConfigPath is used when -config is not given.
func defaultConfigPath() string {
	return config.DefaultConfigPath()
}

// CleanupStaleTempFiles removes atomic-save leftovers (*.tmp-*). A relaunch mid-
// flush leaves a zero-byte tmp file behind; nothing else will ever claim it, so
// the working directory slowly fills with them. Files newer than maxAge stay -
// a flush may genuinely be in progress right now.
func CleanupStaleTempFiles(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	deadline := time.Now().Add(-maxAge)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.Contains(name, ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(deadline) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.json")
	addr := flag.String("addr", "", "listen address (overrides the config value)")
	flag.Parse()

	if runtime.GOOS == "windows" {
		// A windowsgui build has no console: lifecycle logs must land in the
		// state dir or a scheduled task's failures are invisible. The file
		// goes first so a failing stderr write cannot swallow it.
		dir := config.DefaultStateDir()
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if f, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				log.SetOutput(io.MultiWriter(f, os.Stderr))
			}
		}
	}

	loader, err := config.NewConfigLoader(*configPath)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}

	// Relaunches that died mid-flush leave zero-byte tmp files behind; the
	// next start is the only moment nothing claims them.
	CleanupStaleTempFiles(filepath.Dir(*configPath), time.Hour)

	modelsPath := filepath.Join(filepath.Dir(*configPath), "models.json")
	registry, existed, err := loadAndValidateRegistry(modelsPath, loader.Get().Models)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	if !existed {
		log.Printf("opencode-proxy: models.json не найден — резолвинг через живой каталог; make install скопирует файл")
	}

	logger, err := reqlog.NewLogger(loader.Get().Log)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	defer logger.Close()

	cfg := loader.Get()
	dialer := &net.Dialer{Timeout: time.Duration(cfg.ConnectTimeoutSeconds) * time.Second}
	p := proxy.NewProxy(loader, registry, logger, &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	})

	mux := proxy.Router(p)

	// Session cost must survive a relaunch: the desktop session outlives the
	// proxy process, so a zeroed counter would read as a bug.
	costs, err := coststore.NewCostStore(cfg.SessionCostFile)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	p.SetCostStore(costs)

	// The quota ledger turns accumulated allowance into the 5-hour/weekly
	// headers Claude Code shows as plan usage.
	usage, err := quota.NewUsageLedger(cfg.UsageFile)
	if err != nil {
		log.Fatalf("opencode-proxy: %v", err)
	}
	p.SetUsageLedger(usage)
	p.SetScheduler(scheduler.NewCellScheduler(cfg.Scheduler.MaxConcurrentCells))
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

	// Pick up hand edits to config.json and scheduled models.json
	// regeneration without a restart.
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
				if reg, changed, err := loadAndValidateRegistry(modelsPath, loader.Get().Models); err != nil {
					log.Printf("opencode-proxy: models.json reload failed, keeping the last good one: %v", err)
				} else if changed {
					p.SetRegistry(reg)
					log.Printf("opencode-proxy: reloaded models.json (%d models)", len(reg.All()))
				}
			}
		}
	}()

	// A relaunch must not cut a streaming response in half: stop accepting new
	// connections, let in-flight requests finish, then exit. SIGTERM and
	// POST /admin/shutdown share this path — Windows cannot deliver signals.
	shutdown := make(chan struct{})
	var shutdownOnce sync.Once
	p.SetShutdownHook(func() { shutdownOnce.Do(func() { close(shutdown) }) })

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sig:
		case <-shutdown:
		}
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

// loadRegistryIfChanged re-reads models.json when its mtime moved past last.
// It reports changed=false when the file has not; a read or parse failure is
// an error and leaves the caller's registry untouched.
func loadRegistryIfChanged(path string, last time.Time) (*models.Registry, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.ModTime().After(last) {
		return nil, false, nil
	}
	reg, err := models.LoadRegistry(path)
	if err != nil {
		return nil, false, err
	}
	return reg, true, nil
}

// loadAndValidateRegistry is loadRegistryOrDefault plus the picker-shadowing
// check: a route picker that equals a catalog picker would silently redirect
// traffic (routes resolve before the registry), so the config is rejected
// instead.
func loadAndValidateRegistry(path string, settings models.Settings) (*models.Registry, bool, error) {
	reg, existed, err := loadRegistryOrDefault(path)
	if err != nil {
		return nil, false, err
	}
	if err := settings.ValidateAgainst(reg); err != nil {
		return nil, existed, err
	}
	return reg, existed, nil
}

// regModTime reads the current mtime of the registry file, for the next
// change check. A missing file yields the zero time, so the next successful
// load counts as a change.
func regModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// loadRegistryOrDefault reads models.json; a missing file is fine on first
// run — the proxy starts with an empty registry and the live catalog keeps
// model resolution working. A present-but-broken file is an error.
func loadRegistryOrDefault(path string) (*models.Registry, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return &models.Registry{}, false, nil
		}
		return nil, false, err
	}
	reg, err := models.LoadRegistry(path)
	if err != nil {
		return nil, false, err
	}
	return reg, true, nil
}
