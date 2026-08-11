package handler

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/mindcache-fyi/server/internal/service"
)

// HealthHandler serves liveness and readiness endpoints.
type HealthHandler struct {
	startTime time.Time
	db        *sql.DB
	storage   *service.Storage
	llm       service.LLM
}

// NewHealthHandler creates a HealthHandler with the given dependencies.
func NewHealthHandler(db *sql.DB, storage *service.Storage, llm service.LLM) *HealthHandler {
	return &HealthHandler{
		startTime: time.Now(),
		db:        db,
		storage:   storage,
		llm:       llm,
	}
}

// Welcome returns a plain-text greeting.
// @Summary Welcome endpoint
// @Tags root
// @Produce text/plain
// @Success 200 {string} string "Hello World!"
// @Router / [get]
func (h *HealthHandler) Welcome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("Hello World!"))
}

// Health returns basic liveness information including uptime in seconds.
// @Summary Health check
// @Tags root
// @Produce json
// @Success 200 {object} map[string]any
// @Router /health [get]
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": int(time.Since(h.startTime).Seconds()),
	})
}

// Ready checks all dependencies and reports readiness.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	checks := map[string]bool{}

	if err := h.db.PingContext(ctx); err != nil {
		checks["database"] = false
	} else {
		checks["database"] = true
	}

	checks["storage"] = h.storage.IsAccessible(ctx)
	checks["llm"] = h.llm.IsConfigured()

	for _, ok := range checks {
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not ready",
				"checks": checks,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}
