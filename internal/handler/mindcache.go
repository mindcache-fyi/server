package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/service"
)

// MindcacheHandler handles mindcache CRUD endpoints.
type MindcacheHandler struct {
	svc *service.MindcacheService
}

// NewMindcacheHandler creates a MindcacheHandler.
func NewMindcacheHandler(svc *service.MindcacheService) *MindcacheHandler {
	return &MindcacheHandler{svc: svc}
}

// List returns all mindcaches.
// @Summary List all mindcaches
// @Tags mindcache
// @Produce json
// @Success 200 {object} model.ListResponse
// @Router /v1/api/list [get]
func (h *MindcacheHandler) List(w http.ResponseWriter, r *http.Request) {
	mindcaches, err := h.svc.List(r.Context())
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, model.ListResponse{Mindcaches: mindcaches})
}

// Get returns a single mindcache by ID with its main content.
// @Summary Get a single mindcache with its content
// @Tags mindcache
// @Produce json
// @Param id path string true "Mindcache ID"
// @Success 200 {object} model.GetResponse
// @Failure 404 {object} model.ErrorResponse
// @Router /v1/api/get/{id} [get]
func (h *MindcacheHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := model.ValidateID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	mc, mainContent, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if mc == nil {
		writeError(w, http.StatusNotFound, "mindcache not found")
		return
	}

	writeJSON(w, http.StatusOK, model.GetResponse{
		Mindcache:   *mc,
		MainContent: mainContent,
		Sources:     h.svc.Sources(r.Context(), id),
	})
}

// Create creates a new mindcache from a cached topic.
// @Summary Create a new mindcache from a cached topic
// @Tags mindcache
// @Accept json
// @Produce json
// @Param body body model.CreateRequest true "Topic ID"
// @Success 200 {object} model.CreateResponse
// @Router /v1/api/create [post]
func (h *MindcacheHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TopicID == "" {
		writeError(w, http.StatusBadRequest, "topicId is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	mc, err := h.svc.CreateFromTopic(ctx, req.TopicID)
	if err != nil {
		writeJSON(w, http.StatusOK, model.CreateResponse{Success: false, Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, model.CreateResponse{Success: true, MindcacheID: mc.ID})
}

// Update integrates a topic into an existing mindcache.
// @Summary Append a cached topic to an existing mindcache
// @Tags mindcache
// @Accept json
// @Produce json
// @Param body body model.UpdateRequest true "Mindcache ID and Topic ID"
// @Success 200 {object} model.UpdateResponse
// @Router /v1/api/update [post]
func (h *MindcacheHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MindcacheID == "" {
		writeError(w, http.StatusBadRequest, "mindcacheId is required")
		return
	}
	if req.TopicID == "" {
		writeError(w, http.StatusBadRequest, "topicId is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	_, err := h.svc.Update(ctx, req.MindcacheID, req.TopicID)
	if err != nil {
		writeJSON(w, http.StatusOK, model.UpdateResponse{
			Success:     false,
			MindcacheID: req.MindcacheID,
			Error:       err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, model.UpdateResponse{
		Success:     true,
		MindcacheID: req.MindcacheID,
	})
}

// Delete removes a mindcache by ID.
// @Summary Delete a mindcache
// @Tags mindcache
// @Produce json
// @Param id path string true "Mindcache ID"
// @Success 200 {object} map[string]any
// @Router /v1/api/delete/{id} [delete]
func (h *MindcacheHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := model.ValidateID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	deleted, err := h.svc.Delete(r.Context(), id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "not found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
