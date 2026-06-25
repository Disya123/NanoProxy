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

// settingUpstreamKey is the settings-table key for the upstream NanoGPT
// bearer token. Kept in one place so admin_api.go and main.go can't drift.
const settingUpstreamKey = "upstream.api_key"

// bootstrapUpstreamKey resolves the upstream NanoGPT API key at startup
// with this priority order:
//
//  1. If NANOGPT_API_KEY env is set AND the settings table is empty,
//     seed the env value into settings (one-shot bootstrap).
//  2. Otherwise load whatever value is in settings.
//  3. Otherwise empty — operator must configure via the admin UI.
//
// Returns the value to load into the in-memory KeyProvider. Logs which
// source was used so the operator can see the bootstrap path in logs.
func bootstrapUpstreamKey(st *store.Store) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbVal, dbErr := st.GetSetting(ctx, settingUpstreamKey)
	envVal := os.Getenv("NANOGPT_API_KEY")

	switch {
	case envVal != "" && (dbErr != nil || dbVal.Value == ""):
		// First boot, or previous DB without a key — promote env to DB.
		if err := st.SetSetting(ctx, settingUpstreamKey, envVal); err != nil {
			log.Printf("settings: failed to seed upstream.api_key from env: %v", err)
		} else {
			log.Printf("settings: seeded upstream.api_key from NANOGPT_API_KEY env into DB")
		}
		return envVal

	case dbErr == nil && dbVal.Value != "":
		log.Printf("settings: loaded upstream.api_key from DB (updated %s)",
			time.UnixMilli(dbVal.UpdatedAt).Format("2006-01-02 15:04:05"))
		return dbVal.Value

	default:
		log.Printf("settings: upstream.api_key not set — configure it via the admin dashboard before proxy traffic will work")
		return ""
	}
}

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

	// Resolve upstream API key once at startup. The KeyProvider is shared by
	// both the proxy handler (reads) and the admin Settings endpoint (writes),
	// so admin updates take effect for new upstream requests immediately.
	keys := proxy.NewKeyProvider(bootstrapUpstreamKey(st))

	log.Printf("nano-proxy starting")
	log.Printf("  proxy listener:  %s", cfg.Server.Listen)
	log.Printf("  admin listener:  %s", cfg.Server.AdminListen)
	log.Printf("  upstream:        %s%s", cfg.Upstream.BaseURL, cfg.Upstream.PathPrefix)
	log.Printf("  upstream key:    %s", keyStatus(keys))
	log.Printf("  go runtime:      %s (GOMAXPROCS=%d, NumCPU=%d)",
		runtime.Version(), runtime.GOMAXPROCS(0), runtime.NumCPU())

	rootCtx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Public proxy server: short write timeout is fine because streaming
	// responses flush their own headers and never block on Write.
	proxySrv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      proxyRouter(cfg, st, keys),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Admin server: same shape, separate listener.
	adminSrv := &http.Server{
		Addr:         cfg.Server.AdminListen,
		Handler:      adminRouter(cfg, st, keys),
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
func proxyRouter(cfg config.Config, st *store.Store, keys *proxy.KeyProvider) http.Handler {
	px := proxy.New(cfg, st, keys)
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
func adminRouter(cfg config.Config, st *store.Store, keys *proxy.KeyProvider) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /admin/static/", ui.StaticHandler())

	// Admin session handler (login / logout).
	admin := &auth.AdminHandler{
		Token:        cfg.Admin.Token,
		CookieSecret: []byte(cfg.Admin.CookieSecret),
		CookieTTL:    cfg.Admin.CookieTTL,
	}
	uiH := handlers.NewAdminUI(admin)
	apiH := handlers.NewAdminAPI(st, []byte(cfg.Admin.CookieSecret), keys)

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
	protected.HandleFunc("GET /admin/settings", uiH.Settings)
	protected.HandleFunc("GET /admin/api/keys", apiH.ListKeys)
	protected.HandleFunc("POST /admin/api/keys", apiH.CreateKey)
	protected.HandleFunc("PATCH /admin/api/keys/{id}", apiH.PatchKey)
	protected.HandleFunc("DELETE /admin/api/keys/{id}", apiH.DeleteKey)
	protected.HandleFunc("GET /admin/api/settings", apiH.GetSettings)
	protected.HandleFunc("PUT /admin/api/settings", apiH.UpdateSettings)
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

// keyStatus returns a short, redacted human-readable summary of the upstream
// key state, used in the startup banner and /admin/settings page header.
func keyStatus(k *proxy.KeyProvider) string {
	v := k.Get()
	if v == "" {
		return "NOT SET — configure via /admin/settings"
	}
	if len(v) <= 8 {
		return "set"
	}
	return "set (… " + v[len(v)-4:] + ")"
}