// Package proxy implements the public NanoGPT reverse proxy. It forwards
// /v1/chat/completions to https://nano-gpt.com/api/v1/chat/completions
// while recording per-request metrics in the store.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/local/nano-proxy/internal/auth"
	"github.com/local/nano-proxy/internal/config"
	"github.com/local/nano-proxy/internal/store"
)

// Proxy holds shared dependencies for the public-facing routes.
type Proxy struct {
	Cfg  config.Config
	St   *store.Store
	Keys *KeyProvider

	// upstream is reused across requests; created lazily by getUpstream.
	upstream *http.Client
}

func New(cfg config.Config, st *store.Store, keys *KeyProvider) *Proxy {
	return &Proxy{Cfg: cfg, St: st, Keys: keys}
}

// getUpstream returns a shared HTTP client tuned for SSE streaming.
func (p *Proxy) getUpstream() *http.Client {
	if p.upstream == nil {
		p.upstream = &http.Client{
			Timeout: p.Cfg.Upstream.RequestTimeout,
		}
	}
	return p.upstream
}

// Routes returns an http.Handler with the public proxy routes mounted.
func (p *Proxy) Routes(secret []byte) http.Handler {
	mux := http.NewServeMux()

	authMW := auth.APIKeyMiddleware(p.St, secret)

	// Non-stream + dispatch (non-stream now, stream in step 4)
	mux.Handle("POST /v1/chat/completions", authMW(http.HandlerFunc(p.handleChatCompletions)))

	// Pass-through for other NanoGPT v1 endpoints (models, etc.). Auth still applied.
	mux.Handle("GET /v1/", authMW(http.HandlerFunc(p.handlePassthrough)))
	mux.Handle("GET /v1/models", authMW(http.HandlerFunc(p.handlePassthrough)))

	return mux
}

// requestMeta is the slim subset of a chat-completion request we need for
// telemetry. Streaming is detected from the boolean `stream` field.
type requestMeta struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	key, ok := auth.APIKeyFromContext(r.Context())
	if !ok {
		// AuthMiddleware should have rejected already.
		writeProxyError(w, http.StatusUnauthorized, "unauthorized", "missing api key in context")
		return
	}

	// Check Limits
	limits, err := p.St.CheckKeyLimits(r.Context(), key)
	if err != nil {
		log.Printf("[Proxy] limit check failed for key %d: %v", key.ID, err)
		// fail open or closed? Let's fail open but log it, or fail closed.
		writeProxyError(w, http.StatusInternalServerError, "internal_error", "failed to check limits")
		return
	}
	if limits.Exceeded {
		writeProxyError(w, http.StatusTooManyRequests, "limit_exceeded", "quota exceeded: "+limits.Reason)
		return
	}

	// Cap client body size defensively (config-driven).
	r.Body = http.MaxBytesReader(w, r.Body, p.Cfg.Limits.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "body_read_error", err.Error())
		return
	}

	var meta requestMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	if meta.Model == "" {
		writeProxyError(w, http.StatusBadRequest, "missing_model", "request body must include `model`")
		return
	}

	// Dispatch to stream or non-stream handler.
	if meta.Stream {
		p.handleStream(w, r, key, body, meta.Model)
		return
	}
	p.handleNonStream(w, r, key, body, meta.Model)
}

// handleNonStream performs a blocking upstream call. The response body is
// captured in full so we can extract usage + pricing before flushing it
// back to the client. This is fine for non-stream responses (typically < 64 KB).
func (p *Proxy) handleNonStream(w http.ResponseWriter, r *http.Request,
	key store.APIKey, body []byte, model string) {

	ctx, cancel := context.WithTimeout(r.Context(), p.Cfg.Upstream.RequestTimeout)
	defer cancel()

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.upstreamURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	copyHeaders(upstreamReq.Header, r.Header, "")
	upstreamKey := p.Keys.Get()
	if upstreamKey == "" {
		writeProxyError(w, http.StatusBadGateway, "upstream_key_missing",
			"upstream NanoGPT API key is not configured — set it via the admin dashboard")
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+upstreamKey)
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json")
	upstreamReq.Host = "" // let http.Client set it from URL

	start := time.Now()
	resp, err := p.getUpstream().Do(upstreamReq)
	latencyMS := int(time.Since(start) / time.Millisecond)
	ip, ua := clientMeta(r)
	if err != nil {
		p.recordFailure(r.Context(), key, model, http.StatusBadGateway,
			"upstream_unreachable", err.Error(), latencyMS, false, ip, ua)
		writeProxyError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.recordFailure(r.Context(), key, model, http.StatusBadGateway,
			"upstream_read_error", err.Error(), latencyMS, false, ip, ua)
		writeProxyError(w, http.StatusBadGateway, "upstream_read_error", err.Error())
		return
	}

	// Copy upstream headers (preserve content-type etc.) for the client.
	copyHeaders(w.Header(), resp.Header, "Content-Length", "Content-Encoding")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)

	// Telemetry — fire-and-forget so the client isn't waiting on SQLite.
	go p.recordFromResponse(context.Background(), key, model, false,
		resp.StatusCode, latencyMS, respBody, ip, ua)
}

// handlePassthrough proxies GETs to /v1/* without touching the body. No
// per-request metrics yet (those arrive in a later iteration).
func (p *Proxy) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.Cfg.Upstream.RequestTimeout)
	defer cancel()

	url := p.upstreamURL(strings.TrimPrefix(r.URL.Path, "/v1"))
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	upstreamKey := p.Keys.Get()
	if upstreamKey == "" {
		writeProxyError(w, http.StatusBadGateway, "upstream_key_missing",
			"upstream NanoGPT API key is not configured — set it via the admin dashboard")
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+upstreamKey)
	upstreamReq.Header.Set("Accept", "application/json")

	resp, err := p.getUpstream().Do(upstreamReq)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header, "Content-Length", "Content-Encoding")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// upstreamURL builds the full upstream URL for a path under /api/v1.
func (p *Proxy) upstreamURL(suffix string) string {
	base := strings.TrimRight(p.Cfg.Upstream.BaseURL, "/")
	prefix := strings.TrimRight(p.Cfg.Upstream.PathPrefix, "/")
	if suffix == "" {
		return base + prefix
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return base + prefix + suffix
}

// copyHeaders copies headers from src to dst. Optional skip list can omit
// hop-by-hop or sensitive headers.
func copyHeaders(dst, src http.Header, skip ...string) {
	skipSet := make(map[string]struct{}, len(skip))
	for _, h := range skip {
		skipSet[http.CanonicalHeaderKey(h)] = struct{}{}
	}
	for k, v := range src {
		if _, drop := skipSet[k]; drop {
			continue
		}
		// Drop incoming Authorization so the upstream header (set explicitly) wins.
		if k == "Authorization" {
			continue
		}
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}

// writeProxyError emits an OpenAI-style error JSON.
func writeProxyError(w http.ResponseWriter, code int, etype, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body := map[string]any{
		"error": map[string]any{"type": etype, "message": msg},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// clientMeta extracts the originating IP and user-agent for telemetry.
func clientMeta(r *http.Request) (ip, ua string) {
	ip = r.RemoteAddr
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Use the first hop if a proxy already filled this.
		if i := strings.IndexByte(xff, ','); i > 0 {
			ip = strings.TrimSpace(xff[:i])
		} else {
			ip = strings.TrimSpace(xff)
		}
	}
	ua = r.Header.Get("User-Agent")
	return ip, ua
}

// recordFailure logs a request that never produced a parseable response.
func (p *Proxy) recordFailure(ctx context.Context, key store.APIKey, model string,
	status int, etype, msg string, latencyMS int, stream bool, clientIP, ua string) {
	if _, err := p.St.RecordRequest(ctx, store.RequestRecord{
		APIKeyID:     key.ID,
		Model:        model,
		Stream:       stream,
		StatusCode:   status,
		ErrorType:    etype,
		ErrorMessage: msg,
		LatencyMS:    latencyMS,
		ClientIP:     clientIP,
		UserAgent:    ua,
	}); err != nil {
		log.Printf("record request: %v", err)
	}
}