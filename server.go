// Package server provides the embeddable MindCache server: configuration,
// application wiring, and lifecycle management. The cmd/server binary is a
// thin wrapper around this package driven by environment variables.
package server

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	httpSwagger "github.com/swaggo/http-swagger"

	"github.com/mindcache-fyi/server/internal/cache"
	"github.com/mindcache-fyi/server/internal/handler"
	"github.com/mindcache-fyi/server/internal/service"
	"github.com/mindcache-fyi/server/internal/store"

	_ "github.com/mindcache-fyi/server/docs"
)

// App is a fully wired MindCache server instance.
type App struct {
	cfg     *Config
	srv     *http.Server
	ln      net.Listener
	kv      *cache.KVCache
	storage *service.Storage
	db      *sql.DB
}

// New validates cfg and wires up all server dependencies (database, storage,
// LLM client, services, and the HTTP router). The returned App is not started;
// call Start to begin serving.
func New(cfg *Config) (*App, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	if err := store.RunMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	kv := cache.NewKVCache()

	storage, err := service.NewStorage(context.Background(), cfg.StorageURL)
	if err != nil {
		kv.Stop()
		_ = db.Close()
		return nil, err
	}

	llm, err := service.NewLLMClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMMaxConcurrency, cfg.IsDev())
	if err != nil {
		_ = storage.Close()
		kv.Stop()
		_ = db.Close()
		return nil, err
	}

	repo := store.NewMindcacheRepo(db)
	analyseSvc := service.NewAnalyseService(kv, repo, llm, cfg.LLMMaxInputChars)
	mindcacheSvc := service.NewMindcacheService(repo, storage, llm, kv, cfg.LLMMaxInputChars)

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

	// When static content is injected (e.g. an embedded SPA), it takes over
	// the site root and the welcome text endpoint is omitted.
	if cfg.PublicFS == nil {
		r.Get("/", healthH.Welcome)
	}
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

	switch {
	case cfg.PublicFS != nil:
		r.NotFound(http.FileServerFS(cfg.PublicFS).ServeHTTP)
	default:
		if _, err := os.Stat("public"); err == nil {
			r.NotFound(http.FileServer(http.Dir("public")).ServeHTTP)
		}
	}

	return &App{
		cfg:     cfg,
		srv:     &http.Server{Handler: r},
		kv:      kv,
		storage: storage,
		db:      db,
	}, nil
}

// Start binds the listen port and serves in the background. It returns as
// soon as the port is bound; a bind failure (e.g. port already in use) is
// returned to the caller.
func (a *App) Start() error {
	ln, err := net.Listen("tcp", ":"+a.cfg.Port)
	if err != nil {
		return err
	}
	a.ln = ln

	go func() {
		if err := a.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
		}
	}()

	slog.Info("server listening", "addr", ln.Addr().String(), "env", a.cfg.Env)
	return nil
}

// Addr returns the bound listen address, or an empty string when the App
// has not been started.
func (a *App) Addr() string {
	if a.ln == nil {
		return ""
	}
	return a.ln.Addr().String()
}

// Port returns the configured port.
func (a *App) Port() string {
	return a.cfg.Port
}

// Shutdown gracefully stops the HTTP server and releases the cache, storage,
// and database.
func (a *App) Shutdown(ctx context.Context) error {
	slog.Info("server shutting down")
	err := a.srv.Shutdown(ctx)
	a.kv.Stop()
	if cerr := a.storage.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if derr := a.db.Close(); derr != nil && err == nil {
		err = derr
	}
	return err
}
