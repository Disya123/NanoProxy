package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// CookieName is the name of the admin session cookie.
const CookieName = "np_admin"

// AdminHandler bundles the admin auth endpoints (login / logout).
type AdminHandler struct {
	Token       string        // plaintext token the operator types in the login form
	CookieSecret []byte       // HMAC key for signing session cookies
	CookieTTL   time.Duration // session lifetime
	Secure      bool          // emit Secure flag (set true behind TLS)
}

// LoginRequest is the body of POST /admin/api/login.
type LoginRequest struct {
	Token string `json:"token"`
}

// Login verifies the operator token and, on success, sets a signed session
// cookie. Constant-time compare prevents trivial timing leaks.
func (h *AdminHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "JSON body required")
		return
	}
	if req.Token == "" || !hmac.Equal([]byte(req.Token), []byte(h.Token)) {
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", "wrong admin token")
		return
	}
	exp := time.Now().Add(h.CookieTTL).Unix()
	value := h.sign(exp)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/admin/",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// Logout expires the session cookie.
func (h *AdminHandler) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/admin/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// Middleware verifies the admin session cookie on every /admin/* request
// except those explicitly marked public (login / static / healthz).
func (h *AdminHandler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil {
			// Redirect HTML requests to the login page; return JSON otherwise.
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "text/html") && r.Method == http.MethodGet {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		expUnix, ok := h.verify(c.Value)
		if !ok || time.Now().Unix() > expUnix {
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "text/html") && r.Method == http.MethodGet {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			writeJSONError(w, http.StatusUnauthorized, "session_expired", "please log in again")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sign returns "<exp>.<hex-hmac>" for the given expiry.
func (h *AdminHandler) sign(exp int64) string {
	mac := hmac.New(sha256.New, h.CookieSecret)
	mac.Write([]byte(itob(exp)))
	return itob(exp) + "." + hex.EncodeToString(mac.Sum(nil))
}

// verify returns (exp, true) if the cookie value's signature is valid.
// It does NOT check expiry; the caller does that.
func (h *AdminHandler) verify(value string) (int64, bool) {
	dot := strings.IndexByte(value, '.')
	if dot <= 0 || dot >= len(value)-1 {
		return 0, false
	}
	expStr := value[:dot]
	sigHex := value[dot+1:]
	mac := hmac.New(sha256.New, h.CookieSecret)
	mac.Write([]byte(expStr))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sigHex)) {
		return 0, false
	}
	exp, err := atoi64(expStr)
	if err != nil {
		return 0, false
	}
	return exp, true
}

func itob(n int64) string {
	// Avoid pulling in strconv for a 1-liner; unix times fit in 10 digits.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func atoi64(s string) (int64, error) {
	var n int64
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, errAtoi
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

var errAtoi = &atoiError{}

type atoiError struct{}

func (*atoiError) Error() string { return "invalid integer" }