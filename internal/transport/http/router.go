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

	// ---- department roster (any member of that department, or super) ----
	// Lets a department dashboard build its staff / PIC list from the central
	// identity store instead of a local seed.
	mux.HandleFunc("GET /api/dept/{dept}/users", h.requireAuth(h.deptUsers))
	// ---- department catalogue (any logged-in user) ----
	// Lets a dashboard build its "output to division" list from the central
	// department catalogue instead of a hardcoded list.
	mux.HandleFunc("GET /api/departments", h.requireAuth(h.listDepartments))

	// ---- messaging / Chat (any logged-in user): DMs + per-division channels ----
	mux.HandleFunc("GET /api/messages/users", h.requireAuth(h.chatUsers))
	mux.HandleFunc("GET /api/messages/conversations", h.requireAuth(h.chatConversations))
	mux.HandleFunc("GET /api/messages/unread", h.requireAuth(h.chatUnread))
	mux.HandleFunc("GET /api/messages/with/{userId}", h.requireAuth(h.chatThread))
	mux.HandleFunc("POST /api/messages/with/{userId}", h.requireAuth(h.chatSend))
	mux.HandleFunc("POST /api/messages/with/{userId}/file", h.requireAuth(h.chatSendFile))
	mux.HandleFunc("POST /api/messages/with/{userId}/read", h.requireAuth(h.chatRead))
	// Per-division channels (group chat scoped to a department's members).
	mux.HandleFunc("GET /api/messages/channels", h.requireAuth(h.chatChannels))
	mux.HandleFunc("GET /api/messages/channel/{code}", h.requireAuth(h.chatChannelThread))
	mux.HandleFunc("POST /api/messages/channel/{code}", h.requireAuth(h.chatChannelSend))
	mux.HandleFunc("POST /api/messages/channel/{code}/file", h.requireAuth(h.chatChannelSendFile))
	mux.HandleFunc("POST /api/messages/channel/{code}/read", h.requireAuth(h.chatChannelRead))
	// Attachment download (participant / channel-member only).
	mux.HandleFunc("GET /api/messages/file/{attId}", h.requireAuth(h.chatFile))
	// SSE realtime stream; token comes as a query param (EventSource can't set headers).
	mux.HandleFunc("GET /api/messages/stream", h.chatStream)

	// ---- cross-division AI assistant (any logged-in dashboard user) ----
	mux.HandleFunc("GET /api/ai/config", h.requireAuth(h.aiConfigGet))
	mux.HandleFunc("PUT /api/ai/config", h.requireAuth(h.aiConfigSet))
	mux.HandleFunc("POST /api/ai/chat", h.requireAuth(h.aiChat))
	// Vision proxy — central key stays here; perencanaan Deep Revisi calls this.
	mux.HandleFunc("POST /api/ai/vision", h.requireAuth(h.aiVision))
	// ---- AI orchestrator: 5-stage cross-division pipeline (directors) ----
	mux.HandleFunc("POST /api/ai/orchestrate", h.requireAuth(h.aiOrchestrate))
	// ---- Meta Ads multi-agent PKPSICOV generator (marketing) ----
	// meta-plan: AI designs the expert panel (dynamic count); meta-agent runs one.
	mux.HandleFunc("POST /api/ai/meta-plan", h.requireAuth(h.aiMetaPlan))
	mux.HandleFunc("POST /api/ai/meta-agent", h.requireAuth(h.aiMetaAgent))
	// ---- Deep Analysis: research pipeline with web tools (marketing) ----
	// deep-plan designs up to 9 research agents (+ synthesis = max 10 total);
	// deep-agent runs one agent with a server-side search/open tool loop,
	// governed by the skill markdown in dashboard/skillmd (deep-skills lists them).
	mux.HandleFunc("POST /api/ai/deep-plan", h.requireAuth(h.aiDeepPlan))
	mux.HandleFunc("POST /api/ai/deep-agent", h.requireAuth(h.aiDeepAgent))
	mux.HandleFunc("GET /api/ai/deep-skills", h.requireAuth(h.aiDeepSkills))
	// ---- Generic per-division multi-agent analysis ("Generate AI" on every
	// dashboard). Same shape as meta-* but division-agnostic: analyze-plan designs
	// the expert panel for a division snapshot; analyze-agent runs one expert (or
	// the built-in synthesis finalizer that returns the executive dashboard JSON).
	mux.HandleFunc("POST /api/ai/analyze-plan", h.requireAuth(h.aiAnalyzePlan))
	mux.HandleFunc("POST /api/ai/analyze-agent", h.requireAuth(h.aiAnalyzeAgent))
	mux.HandleFunc("GET /api/ai/models", h.requireAuth(h.aiModels))

	// ---- administration (super only) ----
	// Master data: departments (divisi) + role catalogue.
	mux.HandleFunc("GET /api/admin/departments", h.requireSuper(h.listDepartments))
	mux.HandleFunc("POST /api/admin/departments", h.requireSuper(h.createDepartment))
	mux.HandleFunc("DELETE /api/admin/departments/{code}", h.requireSuper(h.deleteDepartment))
	mux.HandleFunc("GET /api/admin/roles", h.requireSuper(h.listRoles))
	mux.HandleFunc("POST /api/admin/roles", h.requireSuper(h.createRole))
	mux.HandleFunc("DELETE /api/admin/roles/{value}", h.requireSuper(h.deleteRole))
	mux.HandleFunc("GET /api/admin/users", h.requireSuper(h.listUsers))
	mux.HandleFunc("POST /api/admin/users", h.requireSuper(h.createUser))
	mux.HandleFunc("GET /api/admin/users/{id}", h.requireSuper(h.getUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", h.requireSuper(h.updateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", h.requireSuper(h.deleteUser))
	mux.HandleFunc("PUT /api/admin/users/{id}/roles/{dept}", h.requireSuper(h.setRole))
	mux.HandleFunc("DELETE /api/admin/users/{id}/roles/{dept}", h.requireSuper(h.removeRole))

	return chain(mux, logger, cors(allowOrigins))
}
