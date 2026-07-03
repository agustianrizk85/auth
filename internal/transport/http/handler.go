package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"greenpark/auth/internal/ai"
	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/repository"
	"greenpark/auth/internal/service"
	"greenpark/auth/internal/token"
)

// Handler holds the dependencies for the HTTP handlers.
type Handler struct {
	auth   *service.Auth
	users  *service.Users
	signer *token.Signer
	ai     *ai.Client // cross-division dashboard assistant (nil = disabled)
}

// NewHandler creates a Handler bound to the services and the token signer.
func NewHandler(auth *service.Auth, users *service.Users, signer *token.Signer) *Handler {
	return &Handler{auth: auth, users: users, signer: signer}
}

// SetAI attaches the OpenRouter chat client that backs POST /api/ai/chat.
// Kept separate from NewHandler so existing call sites (and tests) are unchanged.
func (h *Handler) SetAI(c *ai.Client) { h.ai = c }

/* ---------------------------- auth plumbing ---------------------------- */

type ctxKey int

const claimsCtxKey ctxKey = 0

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// requireAuth rejects requests without a valid (unexpired, correctly signed)
// access token, stashing the verified claims in the request context.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.auth.Verify(bearer(r))
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey, claims)))
	}
}

// requireSuper requires a valid access token belonging to a super user (the
// only role permitted to administer accounts).
func (h *Handler) requireSuper(next http.HandlerFunc) http.HandlerFunc {
	return h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := r.Context().Value(claimsCtxKey).(token.Claims); !ok || !c.Super {
			writeError(w, http.StatusForbidden, "butuh akses superadmin")
			return
		}
		next(w, r)
	})
}

func claimsOf(r *http.Request) token.Claims {
	c, _ := r.Context().Value(claimsCtxKey).(token.Claims)
	return c
}

// decode reads the JSON request body into a value of type T.
func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeError(w, http.StatusBadRequest, "body JSON tidak valid: "+err.Error())
		return v, false
	}
	return v, true
}

// statusForServiceErr maps known service/repository errors to HTTP codes.
func statusForServiceErr(err error) int {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, repository.ErrUsernameTaken):
		return http.StatusConflict
	case errors.Is(err, service.ErrUsernameRequired),
		errors.Is(err, service.ErrPasswordRequired),
		errors.Is(err, service.ErrPasswordTooShort),
		errors.Is(err, service.ErrInvalidRole),
		errors.Is(err, service.ErrUnknownDept):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

/* ---------------------------- public endpoints ---------------------------- */

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "auth"})
}

// jwks publishes the public verification key so departments can validate access
// tokens locally. Served at /.well-known/jwks.json.
func (h *Handler) jwks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, token.JWKS(h.signer.Public(), h.signer.KID()))
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[loginReq](w, r)
	if !ok {
		return
	}
	tokens, err := h.auth.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

type refreshReq struct {
	RefreshToken string `json:"refreshToken"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[refreshReq](w, r)
	if !ok {
		return
	}
	tokens, err := h.auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[refreshReq](w, r)
	if !ok {
		return
	}
	if err := h.auth.Logout(r.Context(), req.RefreshToken); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

/* --------------------------- session endpoints --------------------------- */

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, err := h.auth.UserByID(r.Context(), claimsOf(r).Subject)
	if err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	u.PasswordHash = ""
	writeJSON(w, http.StatusOK, u)
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.LogoutAll(r.Context(), claimsOf(r).Subject); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

/* ----------------------------- admin: users ------------------------------ */

type userReq struct {
	Username string            `json:"username"`
	Email    string            `json:"email"`
	Name     string            `json:"name"`
	Password string            `json:"password"`
	Super    bool              `json:"super"`
	Active   *bool             `json:"active"`
	Roles    map[string]string `json:"roles"`
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// deptUsers returns the roster of one department. Any authenticated member of
// that department (or a super user) may read it — used by dashboards to build
// their PIC / staff list from the central identity store.
func (h *Handler) deptUsers(w http.ResponseWriter, r *http.Request) {
	dept := r.PathValue("dept")
	c := claimsOf(r)
	if !c.Super {
		if _, ok := c.Roles[dept]; !ok {
			writeError(w, http.StatusForbidden, "bukan anggota departemen")
			return
		}
	}
	users, err := h.users.ListByDept(r.Context(), dept)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[userReq](w, r)
	if !ok {
		return
	}
	u, err := h.users.Create(r.Context(), service.CreateInput{
		Username: req.Username,
		Email:    req.Email,
		Name:     req.Name,
		Password: req.Password,
		Super:    req.Super,
		Active:   req.Active,
		Roles:    toRoleMap(req.Roles),
	})
	if err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

type updateUserReq struct {
	Email    *string           `json:"email"`
	Name     *string           `json:"name"`
	Password *string           `json:"password"`
	Super    *bool             `json:"super"`
	Active   *bool             `json:"active"`
	Roles    map[string]string `json:"roles"`
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[updateUserReq](w, r)
	if !ok {
		return
	}
	in := service.UpdateInput{
		Email:    req.Email,
		Name:     req.Name,
		Password: req.Password,
		Super:    req.Super,
		Active:   req.Active,
	}
	if req.Roles != nil {
		in.Roles = toRoleMap(req.Roles)
	}
	u, err := h.users.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == claimsOf(r).Subject {
		writeError(w, http.StatusBadRequest, "tidak bisa menghapus akun sendiri")
		return
	}
	if err := h.users.Delete(r.Context(), id); err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type roleReq struct {
	Role string `json:"role"`
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[roleReq](w, r)
	if !ok {
		return
	}
	err := h.users.SetRole(r.Context(), r.PathValue("id"), r.PathValue("dept"), domain.Role(req.Role))
	if err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) removeRole(w http.ResponseWriter, r *http.Request) {
	err := h.users.RemoveRole(r.Context(), r.PathValue("id"), r.PathValue("dept"))
	if err != nil {
		writeError(w, statusForServiceErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) listDepartments(w http.ResponseWriter, r *http.Request) {
	depts, err := h.users.Departments(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, depts)
}

// toRoleMap converts the wire string roles into typed domain roles. Invalid
// values pass through unchanged and are rejected by the service validation.
func toRoleMap(in map[string]string) map[string]domain.Role {
	out := make(map[string]domain.Role, len(in))
	for dept, role := range in {
		out[dept] = domain.Role(role)
	}
	return out
}
