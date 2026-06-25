// Package handlers wires admin HTTP endpoints (UI pages + JSON API).
package handlers

import (
	"log"
	"net/http"

	"github.com/local/nano-proxy/internal/auth"
	"github.com/local/nano-proxy/internal/ui"
)

// AdminUI serves the dashboard HTML pages. The auth.Middleware is applied
// outside this handler at mount time, so all handlers here assume an
// authenticated operator.
type AdminUI struct {
	Admin *auth.AdminHandler
}

// NewAdminUI constructs the page handler.
func NewAdminUI(admin *auth.AdminHandler) *AdminUI { return &AdminUI{Admin: admin} }

// Login renders the login form (unauthenticated).
func (h *AdminUI) Login(w http.ResponseWriter, r *http.Request) {
	if err := ui.RenderPage(w, "login", nil); err != nil {
		log.Printf("render login: %v", err)
	}
}

// Dashboard renders the overview page.
func (h *AdminUI) Dashboard(w http.ResponseWriter, r *http.Request) {
	if err := ui.RenderPage(w, "dashboard", nil); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

// Requests renders the requests list page.
func (h *AdminUI) Requests(w http.ResponseWriter, r *http.Request) {
	if err := ui.RenderPage(w, "requests", nil); err != nil {
		log.Printf("render requests: %v", err)
	}
}

// Keys renders the client-keys CRUD page.
func (h *AdminUI) Keys(w http.ResponseWriter, r *http.Request) {
	if err := ui.RenderPage(w, "keys", nil); err != nil {
		log.Printf("render keys: %v", err)
	}
}