package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/KAVA-Leigh/addresstify/internal/db"
	"github.com/KAVA-Leigh/addresstify/internal/gnaf"
)

// Handler holds dependencies for all HTTP handlers.
type Handler struct {
	store *db.Store
}

// New creates a new Handler with the given store.
func New(store *db.Store) *Handler {
	return &Handler{store: store}
}

// Search handles GET /addresses?q=...&limit=N
// Returns a list of matching addresses ordered by relevance.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "q parameter is required")
		return
	}

	limit := 10
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	results, err := h.store.Search(r.Context(), q, limit)
	if err != nil {
		slog.Error("search failed", "query", q, "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}

	if results == nil {
		results = []gnaf.Address{}
	}

	writeJSON(w, http.StatusOK, gnaf.SearchResponse{
		Results: results,
		Total:   len(results),
		Query:   q,
	})
}

// Validate handles POST /addresses/validate
// Accepts a structured address and returns the best GNAF match with confidence.
func (h *Handler) Validate(w http.ResponseWriter, r *http.Request) {
	var req gnaf.ValidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	match, confidence, err := h.store.Validate(r.Context(), req)
	if err != nil {
		slog.Error("validate failed", "request", req, "error", err)
		writeError(w, http.StatusInternalServerError, "validate failed")
		return
	}

	writeJSON(w, http.StatusOK, gnaf.ValidateResponse{
		Match:      match,
		Confidence: confidence,
		Matched:    match != nil && confidence >= 0.5,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
