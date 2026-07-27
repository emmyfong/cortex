package httpx

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/emmyf/cortex/backend/internal/store"
)

// SearchHandler answers semantic search queries.
type SearchHandler struct {
	Store    *store.Store
	Embedder *embed.Client
	Logger   *slog.Logger
	DefaultK int
	MaxK     int
}

type searchRequest struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

type searchResponse struct {
	Query   string               `json:"query"`
	Results []store.SearchResult `json:"results"`
}

// Search embeds the query and returns the nearest chunks.
func (h *SearchHandler) Search() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		req.Query = strings.TrimSpace(req.Query)
		if req.Query == "" {
			writeError(w, http.StatusBadRequest, "query is required")
			return
		}

		k := req.K
		if k <= 0 {
			k = h.DefaultK
		}
		if k > h.MaxK {
			// Clamp rather than reject: the caller gets useful results and the
			// database is protected from an unbounded scan.
			k = h.MaxK
		}

		// The query must be embedded by the same model that embedded the
		// chunks, or the vectors are not comparable.
		vector, err := h.Embedder.Embed(r.Context(), req.Query)
		if err != nil {
			h.Logger.Error("could not embed query", slog.String("error", err.Error()))
			writeError(w, http.StatusServiceUnavailable, "could not embed query; is Ollama running?")
			return
		}

		results, err := h.Store.SearchSimilar(r.Context(), vector, k)
		if err != nil {
			h.Logger.Error("search failed", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "search failed")
			return
		}
		if results == nil {
			results = []store.SearchResult{}
		}

		writeJSON(w, http.StatusOK, searchResponse{Query: req.Query, Results: results})
	}
}
