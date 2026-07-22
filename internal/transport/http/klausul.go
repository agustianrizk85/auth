package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"greenpark/auth/internal/klausul"
	"greenpark/auth/internal/token"
)

// canEditDivision allows a superadmin, or a user holding any role in the given
// division (its Kadep), to edit that division's clauses. Same rule as the
// per-division AI model picker.
func (h *Handler) canEditDivision(r *http.Request, div string) bool {
	c, _ := r.Context().Value(claimsCtxKey).(token.Claims)
	if c.Super {
		return true
	}
	_, ok := c.Roles[strings.ToLower(strings.TrimSpace(div))]
	return ok
}

// klausulList returns clauses filtered by ?division= and optional ?docType=.
// Read-only for any logged-in dashboard user.
func (h *Handler) klausulList(w http.ResponseWriter, r *http.Request) {
	if h.clauses == nil {
		writeJSON(w, http.StatusOK, []klausul.Klausul{})
		return
	}
	div := r.URL.Query().Get("division")
	dt := r.URL.Query().Get("docType")
	writeJSON(w, http.StatusOK, h.clauses.List(div, dt))
}

// klausulUpsert creates or replaces a clause (super or the division's Kadep).
func (h *Handler) klausulUpsert(w http.ResponseWriter, r *http.Request) {
	if h.clauses == nil {
		writeError(w, http.StatusServiceUnavailable, "store klausul tidak aktif")
		return
	}
	var k klausul.Klausul
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&k); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	if !h.canEditDivision(r, k.Division) {
		writeError(w, http.StatusForbidden, "hanya divisi Anda sendiri yang bisa diatur")
		return
	}
	saved, err := h.clauses.Upsert(k)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// klausulDelete removes a clause by id (super or the division's Kadep).
func (h *Handler) klausulDelete(w http.ResponseWriter, r *http.Request) {
	if h.clauses == nil {
		writeError(w, http.StatusServiceUnavailable, "store klausul tidak aktif")
		return
	}
	id := r.PathValue("id")
	existing, ok := h.clauses.Get(id)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.canEditDivision(r, existing.Division) {
		writeError(w, http.StatusForbidden, "hanya divisi Anda sendiri yang bisa diatur")
		return
	}
	h.clauses.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}
