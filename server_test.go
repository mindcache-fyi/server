package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestNewStartShutdown(t *testing.T) {
	cfg := &Config{
		Env:               "production",
		Port:              "0",
		DBPath:            filepath.Join(t.TempDir(), "test.db"),
		StorageURL:        "mem://",
		LLMBaseURL:        "http://127.0.0.1:9/v1",
		LLMModel:          "test-model",
		LLMMaxConcurrency: 1,
	}

	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	addr := app.Addr()
	if addr == "" {
		t.Fatal("Addr returned empty string after Start")
	}

	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := &Config{
		Env:        "production",
		Port:       "0",
		DBPath:     filepath.Join(t.TempDir(), "test.db"),
		StorageURL: "file://.",
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error for missing LLM config in production")
	}
}
