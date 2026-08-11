package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/service"
)

// AnalyseHandler handles chat analysis endpoints.
type AnalyseHandler struct {
	svc *service.AnalyseService
}

// NewAnalyseHandler creates an AnalyseHandler.
func NewAnalyseHandler(svc *service.AnalyseService) *AnalyseHandler {
	return &AnalyseHandler{svc: svc}
}

// Analyse extracts topics from a chat and matches them to existing mindcaches.
// @Summary Analyse a chat and return matching topics and mindcaches
// @Tags analyse
// @Accept json
// @Produce json
// @Param body body model.AnalyseRequest true "Chat payload"
// @Success 200 {object} model.AnalyseResponse
// @Failure 400 {object} model.ErrorResponse
// @Failure 502 {object} model.ErrorResponse
// @Failure 504 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /v1/api/analyse [post]
func (h *AnalyseHandler) Analyse(w http.ResponseWriter, r *http.Request) {
	var req model.AnalyseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := model.ValidateChat(&req.Chat); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	result, err := h.svc.Analyse(ctx, req.Chat)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, model.AnalyseResponse{
		Topics:            result.Topics,
		TopicMindcacheMap: result.TopicMindcacheMap,
		Mindcaches:        result.Mindcaches,
	})
}

// ClearAnalyse invalidates cached analysis results for a chat.
// @Summary Clear cached analysis result for a chat
// @Tags analyse
// @Accept json
// @Produce json
// @Param body body model.AnalyseRequest true "Chat payload"
// @Success 200 {object} model.ClearAnalyseResponse
// @Router /v1/api/analyse/clear [post]
func (h *AnalyseHandler) ClearAnalyse(w http.ResponseWriter, r *http.Request) {
	var req model.AnalyseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cleared, err := h.svc.ClearCache(r.Context(), req.Chat)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, model.ClearAnalyseResponse{Cleared: cleared})
}
