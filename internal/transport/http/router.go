package http

import "net/http"

// NewRouter wires all routes to the handler and applies global middleware.
//
// Access tiers:
//   - public: health, login, refresh, logout, JWKS (public key)
//   - authenticated (any valid access token): me, logout-all
//   - super only: the user/role administration API
func NewRouter(h *Handler, allowOrigins []string) http.Handler {
	mux := http.NewServeMux()

	// ---- public ----
	mux.HandleFunc("GET /api/health", h.health)
	mux.HandleFunc("GET /.well-known/jwks.json", h.jwks)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/refresh", h.refresh)
	mux.HandleFunc("POST /api/auth/logout", h.logout)

	// ---- authenticated session ----
	mux.HandleFunc("GET /api/auth/me", h.requireAuth(h.me))
	mux.HandleFunc("POST /api/auth/logout-all", h.requireAuth(h.logoutAll))

	// ---- cross-division AI assistant (any logged-in dashboard user) ----
	mux.HandleFunc("GET /api/ai/config", h.requireAuth(h.aiConfigGet))
	mux.HandleFunc("PUT /api/ai/config", h.requireAuth(h.aiConfigSet))
	mux.HandleFunc("POST /api/ai/chat", h.requireAuth(h.aiChat))
	// ---- AI orchestrator: 5-stage cross-division pipeline (directors) ----
	mux.HandleFunc("POST /api/ai/orchestrate", h.requireAuth(h.aiOrchestrate))

	// ---- administration (super only) ----
	mux.HandleFunc("GET /api/admin/departments", h.requireSuper(h.listDepartments))
	mux.HandleFunc("GET /api/admin/users", h.requireSuper(h.listUsers))
	mux.HandleFunc("POST /api/admin/users", h.requireSuper(h.createUser))
	mux.HandleFunc("GET /api/admin/users/{id}", h.requireSuper(h.getUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", h.requireSuper(h.updateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", h.requireSuper(h.deleteUser))
	mux.HandleFunc("PUT /api/admin/users/{id}/roles/{dept}", h.requireSuper(h.setRole))
	mux.HandleFunc("DELETE /api/admin/users/{id}/roles/{dept}", h.requireSuper(h.removeRole))

	return chain(mux, logger, cors(allowOrigins))
}
