package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/mindcache-fyi/server/internal/service"
)

// RequestLogger logs method, path, status, duration, and request ID after each request.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

// writeServiceError maps a service error to an appropriate HTTP status, logs
// it with the request ID, and writes an error response.
func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, service.ErrLLMUpstream), errors.Is(err, service.ErrLLMResponse):
		status = http.StatusBadGateway
	}

	level := slog.LevelError
	if status != http.StatusInternalServerError {
		level = slog.LevelWarn
	}
	slog.Log(r.Context(), level, "request failed",
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"request_id", middleware.GetReqID(r.Context()),
		"error", err,
	)

	msg := err.Error()
	if status == http.StatusGatewayTimeout {
		msg = "operation timed out"
	}
	writeError(w, status, msg)
}

// CORS returns a chi-compatible CORS middleware allowing all origins.
func CORS(next http.Handler) http.Handler {
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
	})
	return c.Handler(next)
}

// BodyLimit restricts request bodies to 50 MB.
func BodyLimit(next http.Handler) http.Handler {
	const maxBodyBytes = 50 << 20
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
