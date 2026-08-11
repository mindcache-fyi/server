package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	httpSwagger "github.com/swaggo/http-swagger"

	"github.com/mindcache-fyi/server/internal/cache"
	"github.com/mindcache-fyi/server/internal/config"
	"github.com/mindcache-fyi/server/internal/handler"
	"github.com/mindcache-fyi/server/internal/service"
	"github.com/mindcache-fyi/server/internal/store"

	_ "github.com/mindcache-fyi/server/docs"
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

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	setupLogger(cfg.IsProd())

	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}

	if err := store.RunMigrations(db); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	kv := cache.NewKVCache()

	ctx := context.Background()
	storage, err := service.NewStorage(ctx, cfg.StorageURL)
	if err != nil {
		slog.Error("failed to open storage", "error", err)
		os.Exit(1)
	}

	llm, err := service.NewLLMClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMMaxConcurrency, cfg.IsDev())
	if err != nil {
		slog.Error("failed to create llm client", "error", err)
		os.Exit(1)
	}

	repo := store.NewMindcacheRepo(db)
	analyseSvc := service.NewAnalyseService(kv, repo, llm)
	mindcacheSvc := service.NewMindcacheService(repo, storage, llm, kv)

	healthH := handler.NewHealthHandler(db, storage, llm)
	analyseH := handler.NewAnalyseHandler(analyseSvc)
	mindcacheH := handler.NewMindcacheHandler(mindcacheSvc)
	debugH := handler.NewDebugHandler(llm, kv)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(handler.RequestLogger)
	r.Use(middleware.Recoverer)
	r.Use(handler.CORS)
	r.Use(handler.BodyLimit)
	r.Use(middleware.Compress(5))

	r.Get("/", healthH.Welcome)
	r.Get("/health", healthH.Health)
	r.Get("/health/ready", healthH.Ready)

	r.Route("/v1/api", func(r chi.Router) {
		r.Post("/analyse", analyseH.Analyse)
		r.Post("/analyse/clear", analyseH.ClearAnalyse)
		r.Get("/list", mindcacheH.List)
		r.Get("/get/{id}", mindcacheH.Get)
		r.Post("/create", mindcacheH.Create)
		r.Post("/update", mindcacheH.Update)
		r.Delete("/delete/{id}", mindcacheH.Delete)
	})

	r.Route("/v1/api/debug", func(r chi.Router) {
		r.Get("/llm", debugH.LLMConfig)
		r.Get("/kvcache", debugH.KVCache)
	})

	r.Get("/apidoc", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/apidoc/index.html", http.StatusMovedPermanently)
	})
	r.Get("/apidoc/*", httpSwagger.Handler(httpSwagger.URL("/apidoc/doc.json")))

	if _, err := os.Stat("public"); err == nil {
		fs := http.FileServer(http.Dir("public"))
		r.NotFound(fs.ServeHTTP)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		slog.Info("server starting",
			"version", Version,
			"commit", GitCommit,
			"port", cfg.Port,
			"env", cfg.Env,
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}
	kv.Stop()
	if err := storage.Close(); err != nil {
		slog.Error("storage close error", "error", err)
	}
	if err := db.Close(); err != nil {
		slog.Error("database close error", "error", err)
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
