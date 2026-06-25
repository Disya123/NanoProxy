// Package auth provides HTTP middleware for client API-key authentication
// (used by the public proxy) and admin session-cookie authentication (used
// by the dashboard).
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/local/nano-proxy/internal/store"
)

// ctxKey is unexported so other packages can't collide with our context keys.
type ctxKey int

const (
	ctxAPIKey ctxKey = iota
	ctxAdminSession
)

// APIKeyMiddleware authenticates the caller via the standard `Authorization:
// Bearer <raw>` header. On success, the resolved store.APIKey is stored in the
// request context for downstream handlers.
//
// Responds with 401 on missing/malformed header and 403 on unknown/disabled keys.
func APIKeyMiddleware(st *store.Store, secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractBearer(r.Header.Get("Authorization"))
			if raw == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing_bearer", "Authorization: Bearer <key> required")
				return
			}
			key, err := st.LookupKeyByHash(r.Context(), secret, raw)
			if err != nil {
				switch err {
				case store.ErrKeyNotFound:
					writeJSONError(w, http.StatusForbidden, "invalid_key", "unknown api key")
				case store.ErrKeyDisabled:
					writeJSONError(w, http.StatusForbidden, "key_disabled", "this api key is disabled")
				default:
					writeJSONError(w, http.StatusInternalServerError, "auth_error", err.Error())
				}
				return
			}
			ctx := context.WithValue(r.Context(), ctxAPIKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// APIKeyFromContext returns the APIKey resolved by APIKeyMiddleware, if any.
func APIKeyFromContext(ctx context.Context) (store.APIKey, bool) {
	k, ok := ctx.Value(ctxAPIKey).(store.APIKey)
	return k, ok
}

func extractBearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func writeJSONError(w http.ResponseWriter, code int, etype, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// Minimal escaping — error strings come from us, not user input.
	_, _ = w.Write([]byte(`{"error":{"type":"` + etype + `","message":"` + escape(msg) + `"}}`))
}

func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ")
	return r.Replace(s)
}