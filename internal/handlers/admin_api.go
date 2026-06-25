package handlers

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/nano-proxy/internal/store"
)

// AdminAPI bundles the JSON endpoints used by the admin dashboard.
// Authentication is enforced by auth.AdminHandler.Middleware at mount time.
type AdminAPI struct {
	St     *store.Store
	Secret []byte // HMAC secret for hashing client keys
}

// NewAdminAPI constructs the JSON handler bundle.
func NewAdminAPI(st *store.Store, secret []byte) *AdminAPI {
	return &AdminAPI{St: st, Secret: secret}
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
		row := map[string]any{
			"id":          k.ID,
			"name":        k.Name,
			"key_prefix":  k.KeyPrefix,
			"enabled":     k.Enabled,
			"budget_usd":  k.BudgetUSD,
			"created_at":  k.CreatedAt.Format(time.RFC3339),
			"created_at_ms": k.CreatedAt.UnixMilli(),
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
	BudgetUSD *float64 `json:"budget_usd,omitempty"`
	ClearBudget bool   `json:"clear_budget,omitempty"`
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
	if req.ClearBudget {
		if err := h.St.SetKeyBudget(r.Context(), id, nil); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "budget_failed", err.Error())
			return
		}
	} else if req.BudgetUSD != nil {
		v := *req.BudgetUSD
		if err := h.St.SetKeyBudget(r.Context(), id, &v); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "budget_failed", err.Error())
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