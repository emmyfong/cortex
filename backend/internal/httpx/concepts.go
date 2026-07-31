package httpx

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/go-chi/chi/v5"
)

// Graph size bounds. A force-graph renderer degrades badly past a few hundred
// nodes, and an unbounded query would scan the whole table.
const (
	defaultGraphNodes = 150
	maxGraphNodes     = 500
)

// ConceptHandler serves the concept graph.
type ConceptHandler struct {
	Store  *store.Store
	Logger *slog.Logger
}

// List returns concepts ordered by how connected they are.
func (h *ConceptHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		concepts, err := h.Store.ListConcepts(r.Context(), 200)
		if err != nil {
			h.Logger.Error("could not list concepts", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not list concepts")
			return
		}
		if concepts == nil {
			concepts = []store.Concept{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"concepts": concepts})
	}
}

// Graph returns nodes and edges for rendering.
func (h *ConceptHandler) Graph() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := defaultGraphNodes
		if raw := r.URL.Query().Get("limit"); raw != "" {
			// A bad limit is clamped rather than rejected: the caller still
			// gets a usable graph instead of an error page.
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = min(parsed, maxGraphNodes)
			}
		}

		graph, err := h.Store.LoadGraph(r.Context(), limit)
		if err != nil {
			h.Logger.Error("could not load graph", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not load graph")
			return
		}
		writeJSON(w, http.StatusOK, graph)
	}
}

// Get returns one concept with the passages that evidence it.
func (h *ConceptHandler) Get() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")

		detail, err := h.Store.GetConcept(r.Context(), slug)
		if err != nil {
			writeError(w, http.StatusNotFound, "concept not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}
