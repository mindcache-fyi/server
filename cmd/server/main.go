package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	server "github.com/mindcache-fyi/server"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
)

// @title Mindcache FYI
// @version 1.0
// @description Agent backend service for knowledge capture and recall.
// @host localhost:9000
// @BasePath /
// @schemes http
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Printf("mindcache-server %s (%s)\n", Version, GitCommit)
			return
		case "--help", "-h":
			fmt.Printf("mindcache-server %s (%s)\n\n", Version, GitCommit)
			fmt.Println("Usage: mindcache-server [options]")
			fmt.Println("\nOptions:")
			fmt.Println("  -h, --help       Show this help message")
			fmt.Println("  -v, --version    Show version information")
			fmt.Println("\nEnvironment variables:")
			fmt.Println("  PORT               Listen port (default: 9000)")
			fmt.Println("  DB_PATH            SQLite database path (default: mindcache.db)")
			fmt.Println("  STORAGE_URL        Blob storage URL (default: file://.)")
			fmt.Println("  LLM_BASE_URL       OpenAI-compatible LLM endpoint")
			fmt.Println("  LLM_API_KEY        LLM API key (default: local)")
			fmt.Println("  LLM_MODEL          LLM model name")
			fmt.Println("  LLM_MAX_CONCURRENCY  Max concurrent LLM calls (default: 1)")
			return
		}
	}

	cfg, err := server.LoadConfigFromEnv()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	setupLogger(cfg.IsProd())

	app, err := server.New(cfg)
	if err != nil {
		slog.Error("failed to initialize server", "error", err)
		os.Exit(1)
	}

	slog.Info("server starting",
		"version", Version,
		"commit", GitCommit,
		"port", cfg.Port,
		"env", cfg.Env,
	)
	if err := app.Start(); err != nil {
		slog.Error("failed to start server", "error", err)
		os.Exit(1)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}
}

func setupLogger(prod bool) {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if prod {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}
