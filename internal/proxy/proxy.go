// Package proxy implements the public NanoGPT reverse proxy. It forwards
// /v1/chat/completions to https://nano-gpt.com/api/v1/chat/completions
// while recording per-request metrics in the store.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
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

	// Models cache
	modelsMu       sync.RWMutex
	modelsCache    []byte
	modelsCacheExp time.Time
}

func New(cfg config.Config, st *store.Store, keys *KeyProvider) *Proxy {
	return &Proxy{Cfg: cfg, St: st, Keys: keys}
}

// getUpstream returns a shared HTTP client tuned for SSE streaming.
func (p *Proxy) getUpstream() *http.Client {
	if p.upstream == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DisableCompression = true
		p.upstream = &http.Client{
			Transport: t,
			Timeout:   p.Cfg.Upstream.RequestTimeout,
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
	mux.Handle("GET /v1/models", authMW(http.HandlerFunc(p.handleModels)))

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

	// Apply per-key sampler config (if any). MergeSamplers may override the
	// stream flag and inject default generation parameters into the body.
	cfg := ParseSamplerConfig(key.SamplerConfig)
	modifiedBody, wasStreamForced, err := MergeSamplers(body, cfg)
	if err != nil {
		// Log but don't fail — the original body is still valid.
		log.Printf("[proxy] sampler merge error for key %d: %v", key.ID, err)
		modifiedBody = body
	}
	meta.Stream = meta.Stream && !wasStreamForced

	// Dispatch to stream or non-stream handler.
	if meta.Stream {
		p.handleStream(w, r, key, modifiedBody, meta.Model)
		return
	}
	p.handleNonStream(w, r, key, modifiedBody, meta.Model, wasStreamForced)
}

// handleNonStream performs a blocking upstream call. The response body is
// captured in full so we can extract usage + pricing before flushing it
// back to the client. This is fine for non-stream responses (typically < 64 KB).
func (p *Proxy) handleNonStream(w http.ResponseWriter, r *http.Request,
	key store.APIKey, body []byte, model string, wasStreamForced bool) {

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

	if wasStreamForced && resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		// Convert non-stream JSON to a single SSE chunk.
		var nr struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Choices []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(respBody, &nr); err == nil && len(nr.Choices) > 0 {
			flusher, _ := w.(http.Flusher)

			// 1. Initial chunk with role
			chunk1 := map[string]any{
				"id":      nr.ID,
				"object":  "chat.completion.chunk",
				"created": nr.Created,
				"model":   nr.Model,
				"choices": []map[string]any{
					{
						"index": nr.Choices[0].Index,
						"delta": map[string]string{
							"role": nr.Choices[0].Message.Role,
						},
						"finish_reason": nil,
					},
				},
			}
			if b, err := json.Marshal(chunk1); err == nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
				if flusher != nil {
					flusher.Flush()
				}
			}

			// 2. Chunks of content
			content := nr.Choices[0].Message.Content
			runes := []rune(content)
			chunkSize := 20
			delayMs := 5 // ~ 4000 chars/s = ~1000 tokens/s

			for i := 0; i < len(runes); i += chunkSize {
				end := i + chunkSize
				if end > len(runes) {
					end = len(runes)
				}
				chunkN := map[string]any{
					"id":      nr.ID,
					"object":  "chat.completion.chunk",
					"created": nr.Created,
					"model":   nr.Model,
					"choices": []map[string]any{
						{
							"index": nr.Choices[0].Index,
							"delta": map[string]string{
								"content": string(runes[i:end]),
							},
							"finish_reason": nil,
						},
					},
				}
				if b, err := json.Marshal(chunkN); err == nil {
					fmt.Fprintf(w, "data: %s\n\n", b)
					if flusher != nil {
						flusher.Flush()
					}
				}
				time.Sleep(time.Duration(delayMs) * time.Millisecond)
			}

			// 3. Final chunk with finish_reason
			chunkLast := map[string]any{
				"id":      nr.ID,
				"object":  "chat.completion.chunk",
				"created": nr.Created,
				"model":   nr.Model,
				"choices": []map[string]any{
					{
						"index":         nr.Choices[0].Index,
						"delta":         map[string]string{},
						"finish_reason": nr.Choices[0].FinishReason,
					},
				},
			}
			if b, err := json.Marshal(chunkLast); err == nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
				fmt.Fprintf(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		} else {
			// Fallback if unmarshal fails
			_, _ = w.Write(respBody)
		}
	} else {
		// Copy upstream headers (preserve content-type etc.) for the client.
		copyHeaders(w.Header(), resp.Header, "Content-Length", "Content-Encoding")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}

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

// handleModels intercepts requests to /v1/models and caches the response.
func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	p.modelsMu.RLock()
	cache := p.modelsCache
	exp := p.modelsCacheExp
	p.modelsMu.RUnlock()

	// If cache is valid, return it instantly
	if cache != nil && time.Now().Before(exp) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cache)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.Cfg.Upstream.RequestTimeout)
	defer cancel()

	url := p.upstreamURL("/models")
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	upstreamKey := p.Keys.Get()
	if upstreamKey == "" {
		writeProxyError(w, http.StatusBadGateway, "upstream_key_missing", "upstream NanoGPT API key is not configured")
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+upstreamKey)
	upstreamReq.Header.Set("Accept", "application/json")

	resp, err := p.getUpstream().Do(upstreamReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		// Serve stale cache as a fallback if upstream fails
		if cache != nil {
			log.Printf("[Proxy] upstream models failed (%v, status=%d), serving stale cache", err, func() int {
				if resp != nil {
					return resp.StatusCode
				}
				return 0
			}())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cache)
			return
		}
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		defer resp.Body.Close()
		copyHeaders(w.Header(), resp.Header, "Content-Length", "Content-Encoding")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if cache != nil {
			log.Printf("[Proxy] upstream models read failed (%v), serving stale cache", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cache)
			return
		}
		writeProxyError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	// Update cache
	p.modelsMu.Lock()
	p.modelsCache = body
	p.modelsCacheExp = time.Now().Add(1 * time.Hour)
	p.modelsMu.Unlock()

	copyHeaders(w.Header(), resp.Header, "Content-Length", "Content-Encoding")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
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
