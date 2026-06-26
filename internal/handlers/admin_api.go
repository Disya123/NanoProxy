package handlers

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/nano-proxy/internal/proxy"
	"github.com/local/nano-proxy/internal/store"
)

// SettingUpstreamKey is the dotted-path key in the settings table for the
// upstream NanoGPT bearer token. Exported so main.go and any CLI tooling
// can reference the same constant.
const SettingUpstreamKey = "upstream.api_key"

// AdminAPI bundles the JSON endpoints used by the admin dashboard.
// Authentication is enforced by auth.AdminHandler.Middleware at mount time.
type AdminAPI struct {
	St     *store.Store
	Secret []byte // HMAC secret for hashing client keys
	Keys   *proxy.KeyProvider
}

// NewAdminAPI constructs the JSON handler bundle.
func NewAdminAPI(st *store.Store, secret []byte, keys *proxy.KeyProvider) *AdminAPI {
	return &AdminAPI{St: st, Secret: secret, Keys: keys}
}

// ───────── Keys CRUD ─────────

type createKeyReq struct {
	Name      string   `json:"name"`
	BudgetUSD *float64 `json:"budget_usd,omitempty"`
}

func (h *AdminAPI) CreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	key, err := h.St.CreateKey(r.Context(), h.Secret, req.Name, req.BudgetUSD)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, key)
}

func (h *AdminAPI) ListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.St.ListKeys(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	// Augment with 30-day spend (read-only convenience).
	range30 := store.Range{From: thirtyDaysAgo().Format("2006-01-02"), To: today().Format("2006-01-02")}
	totals, _ := h.St.TopKeys(r.Context(), range30, 100)
	byID := make(map[int64]store.KeyTotal, len(totals))
	for _, t := range totals {
		byID[t.APIKeyID] = t
	}
		out := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			var samplerCfg any
			if k.SamplerConfig != "" {
				var parsed any
				if err := json.Unmarshal([]byte(k.SamplerConfig), &parsed); err == nil {
					samplerCfg = parsed
				}
			}
			row := map[string]any{
				"id":          k.ID,
				"name":        k.Name,
				"key_prefix":  k.KeyPrefix,
				"enabled":     k.Enabled,
				"budget_usd":  k.BudgetUSD,
				"created_at":  k.CreatedAt.Format(time.RFC3339),
				"created_at_ms": k.CreatedAt.UnixMilli(),
				"raw_key":     k.RawKey,
				"limit_interval":      k.LimitInterval,
				"limit_input_tokens":  k.LimitInputTokens,
				"limit_output_tokens": k.LimitOutputTokens,
				"limit_total_tokens":  k.LimitTotalTokens,
				"sampler_config":      samplerCfg,
			}
		if t, ok := byID[k.ID]; ok {
			row["spend_30d"] = t.CostUSD
			row["requests_30d"] = t.Requests
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

	type patchKeyReq struct {
		Name      *string  `json:"name,omitempty"`
		Enabled   *bool    `json:"enabled,omitempty"`
		
		// Limits
		LimitInterval      *string  `json:"limit_interval,omitempty"`
		BudgetUSD          *float64 `json:"budget_usd,omitempty"`
		ClearBudget        bool     `json:"clear_budget,omitempty"`
		LimitInputTokens   *int64   `json:"limit_input_tokens,omitempty"`
		ClearInputTokens   bool     `json:"clear_input_tokens,omitempty"`
		LimitOutputTokens  *int64   `json:"limit_output_tokens,omitempty"`
		ClearOutputTokens  bool     `json:"clear_output_tokens,omitempty"`
		LimitTotalTokens   *int64   `json:"limit_total_tokens,omitempty"`
		ClearTotalTokens   bool     `json:"clear_total_tokens,omitempty"`

		// Sampler config (JSON object accepted as raw bytes)
		SamplerConfig *json.RawMessage `json:"sampler_config,omitempty"`
	}

func (h *AdminAPI) PatchKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	var req patchKeyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Name != nil {
		if err := h.St.RenameKey(r.Context(), id, *req.Name); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "rename_failed", err.Error())
			return
		}
	}
	if req.Enabled != nil {
		if err := h.St.SetKeyEnabled(r.Context(), id, *req.Enabled); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "toggle_failed", err.Error())
			return
		}
	}

	// Update limits if any limit-related field is provided
	if req.LimitInterval != nil || req.BudgetUSD != nil || req.ClearBudget || 
	   req.LimitInputTokens != nil || req.ClearInputTokens ||
	   req.LimitOutputTokens != nil || req.ClearOutputTokens ||
	   req.LimitTotalTokens != nil || req.ClearTotalTokens {
		
		key, err := h.St.GetKeyByID(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "get_key_failed", err.Error())
			return
		}

		interval := key.LimitInterval
		if req.LimitInterval != nil {
			interval = *req.LimitInterval
		}

		budget := key.BudgetUSD
		if req.ClearBudget {
			budget = nil
		} else if req.BudgetUSD != nil {
			budget = req.BudgetUSD
		}

		in := key.LimitInputTokens
		if req.ClearInputTokens {
			in = nil
		} else if req.LimitInputTokens != nil {
			in = req.LimitInputTokens
		}

		out := key.LimitOutputTokens
		if req.ClearOutputTokens {
			out = nil
		} else if req.LimitOutputTokens != nil {
			out = req.LimitOutputTokens
		}

		tot := key.LimitTotalTokens
		if req.ClearTotalTokens {
			tot = nil
		} else if req.LimitTotalTokens != nil {
			tot = req.LimitTotalTokens
		}

		if err := h.St.UpdateKeyLimits(r.Context(), id, interval, budget, in, out, tot); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "limits_failed", err.Error())
				return
			}
		}

		// Update sampler config (JSON object or null/empty to clear).
		if req.SamplerConfig != nil {
			raw := strings.TrimSpace(string(*req.SamplerConfig))
			if raw == "" || raw == "null" || raw == "{}" {
				raw = ""
			}
			if err := h.St.UpdateKeySamplerConfig(r.Context(), id, raw); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "sampler_config_failed", err.Error())
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AdminAPI) DeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if err := h.St.DeleteKey(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ───────── Stats ─────────

func (h *AdminAPI) Summary(w http.ResponseWriter, r *http.Request) {
	rng := parseRange(r)
	s, err := h.St.Summary(r.Context(), rng)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "summary_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *AdminAPI) TimeSeries(w http.ResponseWriter, r *http.Request) {
	rng := parseRange(r)
	keyID, _ := strconv.ParseInt(r.URL.Query().Get("key"), 10, 64)
	by := r.URL.Query().Get("by")

	if by == "model" {
		pts, err := h.St.TimeSeriesModels(r.Context(), rng, keyID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "series_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, pts)
		return
	}

	pts, err := h.St.TimeSeries(r.Context(), rng, keyID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "series_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

func (h *AdminAPI) Breakdown(w http.ResponseWriter, r *http.Request) {
	rng := parseRange(r)
	by := r.URL.Query().Get("by")
	switch by {
	case "key":
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := h.St.TopKeys(r.Context(), rng, limit)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "breakdown_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case "model", "":
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := h.St.TopModels(r.Context(), rng, limit)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "breakdown_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rows)
	default:
		writeAPIError(w, http.StatusBadRequest, "bad_by", "by must be 'key' or 'model'")
	}
}

// ───────── Requests ─────────

func (h *AdminAPI) ListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListRequestsFilter{
		Model: q.Get("model"),
	}
	if v := q.Get("key"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			f.APIKeyID = id
		}
	}
	if v := q.Get("status_min"); v != "" {
		f.StatusMin, _ = strconv.Atoi(v)
	}
	if v := q.Get("status_max"); v != "" {
		f.StatusMax, _ = strconv.Atoi(v)
	}
	if v := q.Get("from"); v != "" {
		f.FromMS = dateToMS(v, false)
	}
	if v := q.Get("to"); v != "" {
		f.ToMS = dateToMS(v, true)
	}
	if q.Get("has_tool_error") == "1" {
		t := true
		f.HasError = &t
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))

	rows, total, err := h.St.ListRequests(r.Context(), f)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  rows,
		"total":  total,
		"limit":  clampLimit(f.Limit),
		"offset": f.Offset,
	})
}

func (h *AdminAPI) ExportRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListRequestsFilter{Model: q.Get("model")}
	if v := q.Get("key"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		f.APIKeyID = id
	}
	if v := q.Get("status_min"); v != "" {
		f.StatusMin, _ = strconv.Atoi(v)
	}
	if v := q.Get("status_max"); v != "" {
		f.StatusMax, _ = strconv.Atoi(v)
	}
	if v := q.Get("from"); v != "" {
		f.FromMS = dateToMS(v, false)
	}
	if v := q.Get("to"); v != "" {
		f.ToMS = dateToMS(v, true)
	}
	if q.Get("has_tool_error") == "1" {
		t := true
		f.HasError = &t
	}
	f.Limit = 5000
	rows, _, err := h.St.ListRequests(r.Context(), f)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "export_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="nano-proxy-requests-`+time.Now().UTC().Format("20060102-150405")+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "ts", "key", "model", "stream", "status_code",
		"prompt_tokens", "completion_tokens", "cached_tokens", "total_tokens",
		"cost_usd", "payment_source", "latency_ms",
		"has_tool_calls", "tool_calls_count", "tool_error", "tool_error_msg",
		"client_ip", "user_agent", "error_type",
	})
	for _, r := range rows {
		_ = cw.Write([]string{
			strconv.FormatInt(r.ID, 10),
			strconv.FormatInt(r.TS.UnixMilli(), 10),
			r.APIKeyName,
			r.Model,
			strconv.FormatBool(r.Stream),
			strconv.Itoa(r.StatusCode),
			strconv.Itoa(r.PromptTokens),
			strconv.Itoa(r.CompletionTokens),
			strconv.Itoa(r.CachedTokens),
			strconv.Itoa(r.TotalTokens),
			strconv.FormatFloat(r.CostUSD, 'f', 6, 64),
			r.PaymentSource,
			strconv.Itoa(r.LatencyMS),
			strconv.FormatBool(r.HasToolCalls),
			strconv.Itoa(r.ToolCallsCount),
			strconv.FormatBool(r.ToolError),
			r.ToolErrorMsg,
			r.ClientIP,
			r.UserAgent,
			r.ErrorType,
		})
	}
	cw.Flush()
}

func (h *AdminAPI) GetRequest(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	rr, err := h.St.GetRequest(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "request not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// Filters powers the dropdowns on the requests page.
func (h *AdminAPI) Filters(w http.ResponseWriter, r *http.Request) {
	keys, err := h.St.ListKeys(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "filters_failed", err.Error())
		return
	}
	models, err := h.St.DistinctModels(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "filters_failed", err.Error())
		return
	}
	out := map[string]any{
		"keys":   keys,
		"models": models,
	}
	writeJSON(w, http.StatusOK, out)
}

// ─────────────────────────── helpers ───────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, code int, etype, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"type": etype, "message": msg},
	})
}

func pathID(r *http.Request, key string) (int64, error) {
	// Go 1.22 ServeMux sets PathValue(key) for patterns like /foo/{id}.
	if v := r.PathValue(key); v != "" {
		return strconv.ParseInt(v, 10, 64)
	}
	// Fallback for direct mux.HandleFunc usage.
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, p := range parts {
		if p == key && i+1 < len(parts) {
			return strconv.ParseInt(parts[i+1], 10, 64)
		}
	}
	return 0, errors.New("missing id")
}

func parseRange(r *http.Request) store.Range {
	q := r.URL.Query()
	return store.Range{From: q.Get("from"), To: q.Get("to")}
}

// dateToMS converts "YYYY-MM-DD" to unix-ms at UTC midnight (start or end).
func dateToMS(s string, endOfDay bool) int64 {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0
	}
	if endOfDay {
		t = t.Add(24 * time.Hour)
	}
	return t.UnixMilli()
}

func clampLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 500 {
		return 500
	}
	return n
}

func today() time.Time    { t := time.Now().UTC(); return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
func thirtyDaysAgo() time.Time { return today().AddDate(0, 0, -30) }

// ───────── Settings (upstream API key, etc.) ─────────
//
// The upstream NanoGPT bearer token is stored in the settings table and
// mirrored into a process-wide KeyProvider so the proxy can pick up a
// rotation without restarting. The Settings page and API never echo the
// full secret back to the client — only a masked preview + metadata.

type settingsView struct {
	UpstreamKeySet   bool   `json:"upstream_key_set"`
	UpstreamKeyLast4 string `json:"upstream_key_last4,omitempty"`
	UpstreamKeyMS    int64  `json:"upstream_key_updated_ms,omitempty"`
}

func (h *AdminAPI) GetSettings(w http.ResponseWriter, r *http.Request) {
	v := h.Keys.Get()
	view := settingsView{UpstreamKeySet: v != ""}
	if v != "" {
		if len(v) > 4 {
			view.UpstreamKeyLast4 = v[len(v)-4:]
		}
		if row, err := h.St.GetSetting(r.Context(), SettingUpstreamKey); err == nil {
			view.UpstreamKeyMS = row.UpdatedAt
		}
	}
	writeJSON(w, http.StatusOK, view)
}

type updateSettingsReq struct {
	UpstreamKey *string `json:"upstream_api_key,omitempty"`
	ClearKey    bool    `json:"clear_upstream_api_key,omitempty"`
}

func (h *AdminAPI) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	switch {
	case req.ClearKey:
		if err := h.St.DeleteSetting(r.Context(), SettingUpstreamKey); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
			return
		}
		h.Keys.Set("")
		log.Printf("settings: upstream.api_key cleared via admin UI — proxy will 502 until a new key is set")

	case req.UpstreamKey != nil:
		key := strings.TrimSpace(*req.UpstreamKey)
		if key == "" {
			writeAPIError(w, http.StatusBadRequest, "empty_key", "upstream_api_key must be non-empty (use clear_upstream_api_key=true to remove)")
			return
		}
		if err := h.St.SetSetting(r.Context(), SettingUpstreamKey, key); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}
		h.Keys.Set(key)
		log.Printf("settings: upstream.api_key updated via admin UI (…%s) — new requests will use the rotated key immediately", tail4(key))
	}

	// Echo the new view back so the page can update without a second round-trip.
	v := h.Keys.Get()
	view := settingsView{UpstreamKeySet: v != ""}
	if v != "" && len(v) > 4 {
		view.UpstreamKeyLast4 = v[len(v)-4:]
	}
	if row, err := h.St.GetSetting(r.Context(), SettingUpstreamKey); err == nil {
		view.UpstreamKeyMS = row.UpdatedAt
	}
	writeJSON(w, http.StatusOK, view)
}

func tail4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}