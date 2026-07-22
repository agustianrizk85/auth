package http

import (
	"encoding/json"
	"net/http"

	"greenpark/auth/internal/kontraktor"
	"greenpark/auth/internal/token"
)

// canWriteMaster allows a superadmin or any user holding a division role (a
// manager) to edit shared master data. Viewers with no role are read-only.
func (h *Handler) canWriteMaster(r *http.Request) bool {
	c, _ := r.Context().Value(claimsCtxKey).(token.Claims)
	return c.Super || len(c.Roles) > 0
}

// kontraktorList returns contractors, optionally filtered by ?q=. Read-only for
// any logged-in dashboard user.
func (h *Handler) kontraktorList(w http.ResponseWriter, r *http.Request) {
	if h.kontraktor == nil {
		writeJSON(w, http.StatusOK, []kontraktor.Kontraktor{})
		return
	}
	writeJSON(w, http.StatusOK, h.kontraktor.List(r.URL.Query().Get("q")))
}

// kontraktorUpsert creates or replaces a contractor (super or any manager).
func (h *Handler) kontraktorUpsert(w http.ResponseWriter, r *http.Request) {
	if h.kontraktor == nil {
		writeError(w, http.StatusServiceUnavailable, "store kontraktor tidak aktif")
		return
	}
	if !h.canWriteMaster(r) {
		writeError(w, http.StatusForbidden, "tidak berwenang mengubah master kontraktor")
		return
	}
	var k kontraktor.Kontraktor
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&k); err != nil {
		writeError(w, http.StatusBadRequest, "body tidak valid: "+err.Error())
		return
	}
	saved, err := h.kontraktor.Upsert(k)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// kontraktorDelete removes a contractor by id (super or any manager).
func (h *Handler) kontraktorDelete(w http.ResponseWriter, r *http.Request) {
	if h.kontraktor == nil {
		writeError(w, http.StatusServiceUnavailable, "store kontraktor tidak aktif")
		return
	}
	if !h.canWriteMaster(r) {
		writeError(w, http.StatusForbidden, "tidak berwenang menghapus master kontraktor")
		return
	}
	h.kontraktor.Delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
