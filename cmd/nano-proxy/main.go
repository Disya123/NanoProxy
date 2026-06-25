// nano-proxy: a reverse proxy for the NanoGPT API with detailed request
// logging, caching/usage tracking, and a built-in admin dashboard.
//
// Two HTTP listeners run side by side:
//   - server.listen      public proxy (clients send OpenAI-compatible requests)
//   - server.admin_listen admin dashboard (keep on localhost / firewall)
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/local/nano-proxy/internal/auth"
	"github.com/local/nano-proxy/internal/config"
	"github.com/local/nano-proxy/internal/handlers"
	"github.com/local/nano-proxy/internal/proxy"
	"github.com/local/nano-proxy/internal/store"
	"github.com/local/nano-proxy/internal/ui"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	// Honour GOMEMLIMIT if the operator set it (recommended on 512 MB VPS).
	if os.Getenv("GOMEMLIMIT") != "" {
		debug.SetMemoryLimit(-1) // disabled; GOMEMLIMIT is read by runtime automatically
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.Admin.CookieSecret == cfg.Admin.Token {
		log.Printf("warning: admin.cookie_secret not set, falling back to admin.token")
	}

	st, err := store.Open(cfg.Storage.DBPath, cfg.Storage.RetentionDays)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	log.Printf("  storage:         %s (retention %dd)", cfg.Storage.DBPath, cfg.Storage.RetentionDays)

	log.Printf("nano-proxy starting")
	log.Printf("  proxy listener:  %s", cfg.Server.Listen)
	log.Printf("  admin listener:  %s", cfg.Server.AdminListen)
	log.Printf("  upstream:        %s%s", cfg.Upstream.BaseURL, cfg.Upstream.PathPrefix)
	log.Printf("  go runtime:      %s (GOMAXPROCS=%d, NumCPU=%d)",
		runtime.Version(), runtime.GOMAXPROCS(0), runtime.NumCPU())

	rootCtx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Public proxy server: short write timeout is fine because streaming
	// responses flush their own headers and never block on Write.
	proxySrv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      proxyRouter(cfg, st),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Admin server: same shape, separate listener.
	adminSrv := &http.Server{
		Addr:         cfg.Server.AdminListen,
		Handler:      adminRouter(cfg, st),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Printf("proxy: listening on %s", cfg.Server.Listen)
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		log.Printf("admin: listening on %s", cfg.Server.AdminListen)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Printf("server error: %v", err)
	case <-rootCtx.Done():
		log.Printf("shutdown signal received")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = proxySrv.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)
	log.Printf("nano-proxy stopped")
}

// proxyRouter wires the public, client-facing routes.
func proxyRouter(cfg config.Config, st *store.Store) http.Handler {
	px := proxy.New(cfg, st)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Public proxy routes — /v1/chat/completions handles both stream and non-stream.
	mux.Handle("/v1/", px.Routes([]byte(cfg.Admin.CookieSecret)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"nano-proxy: not found"}`))
	})
	return mux
}

// adminRouter wires the admin dashboard and JSON API.
func adminRouter(cfg config.Config, st *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /admin/static/", ui.StaticHandler())

	// Admin session handler (login / logout).
	admin := &auth.AdminHandler{
		Token:        cfg.Admin.Token,
		CookieSecret: []byte(cfg.Admin.CookieSecret),
		CookieTTL:    cfg.Admin.CookieTTL,
	}
	uiH := handlers.NewAdminUI(admin)
	apiH := handlers.NewAdminAPI(st, []byte(cfg.Admin.CookieSecret))

	// Public, unauthenticated endpoints.
	mux.HandleFunc("POST /admin/api/login", admin.Login)
	mux.HandleFunc("POST /admin/api/logout", admin.Logout)
	mux.HandleFunc("GET /admin/login", uiH.Login)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","component":"admin"}`))
	})

	// Protected endpoints (everything below /admin/api/* and the HTML pages).
	protected := http.NewServeMux()
	protected.HandleFunc("GET /admin/", uiH.Dashboard)
	protected.HandleFunc("GET /admin/requests", uiH.Requests)
	protected.HandleFunc("GET /admin/keys", uiH.Keys)
	protected.HandleFunc("GET /admin/api/keys", apiH.ListKeys)
	protected.HandleFunc("POST /admin/api/keys", apiH.CreateKey)
	protected.HandleFunc("PATCH /admin/api/keys/{id}", apiH.PatchKey)
	protected.HandleFunc("DELETE /admin/api/keys/{id}", apiH.DeleteKey)
	protected.HandleFunc("GET /admin/api/stats/summary", apiH.Summary)
	protected.HandleFunc("GET /admin/api/stats/timeseries", apiH.TimeSeries)
	protected.HandleFunc("GET /admin/api/stats/breakdown", apiH.Breakdown)
	protected.HandleFunc("GET /admin/api/requests", apiH.ListRequests)
	protected.HandleFunc("GET /admin/api/requests/export", apiH.ExportRequests)
	protected.HandleFunc("GET /admin/api/requests/{id}", apiH.GetRequest)
	protected.HandleFunc("GET /admin/api/filters", apiH.Filters)

	mux.Handle("/", admin.Middleware(protected))
	return mux
}