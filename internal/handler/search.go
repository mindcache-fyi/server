package handler

import (
	"net/http"
	"strconv"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/service"
)

// SearchHandler handles full-text search endpoints.
type SearchHandler struct {
	svc *service.SearchIndexService
}

// NewSearchHandler creates a SearchHandler.
func NewSearchHandler(svc *service.SearchIndexService) *SearchHandler {
	return &SearchHandler{svc: svc}
}

// Search runs a full-text query over mindcache briefs and content.
// @Summary Full-text search over briefs and content
// @Tags search
// @Produce json
// @Param q query string true "Query text"
// @Param limit query int false "Maximum number of results (default 20, max 100)"
// @Success 200 {object} model.SearchResponse
// @Failure 400 {object} model.ErrorResponse
// @Router /v1/api/search [get]
func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}

	limit := service.DefaultSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}

	results, err := h.svc.Search(r.Context(), q, limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, model.SearchResponse{Results: results})
}
