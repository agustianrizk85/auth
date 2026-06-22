// Package http is the HTTP transport for the master auth service: router,
// handlers and middleware. It depends on the service layer only.
package http

import (
	"encoding/json"
	"log"
	"net/http"
)

// errorBody is the JSON envelope returned for error responses.
type errorBody struct {
	Error string `json:"error"`
}

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("auth: encode response: %v", err)
	}
}

// writeError serialises a JSON error with the given status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}
