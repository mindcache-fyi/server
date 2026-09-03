package server

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultSyncInterval is how often the server reconciles local mindcache
// metadata against the meta.json sidecars in the blob bucket.
const DefaultSyncInterval = 60 * time.Second

// Config holds all configuration for a server instance.
type Config struct {
	Env               string
	Port              string
	DBPath            string
	StorageURL        string
	LLMBaseURL        string
	LLMAPIKey         string
	LLMModel          string
	LLMMaxConcurrency int
	// LLMGatewaySalt is the pre-shared HMAC salt used to sign LLM requests
	// for a trusted gateway (e.g. MindCache Try). When empty, requests are
	// unsigned, preserving the original behaviour for user-supplied (BYOK)
	// endpoints.
	LLMGatewaySalt string
	// LLMDeviceID identifies this installation to the gateway's rate limiter
	// and is sent alongside the signature. Only used when LLMGatewaySalt is set.
	LLMDeviceID string
	// LLMMaxInputChars caps conversation text sent to the LLM per call.
	// Values <= 0 disable the cap.
	LLMMaxInputChars int
	// EmbedBaseURL/EmbedAPIKey/EmbedModel configure the optional embedding
	// endpoint used to retrieve matching candidates. EmbedModel empty means
	// the feature is disabled; empty BaseURL/APIKey fall back to the LLM
	// values.
	EmbedBaseURL string
	EmbedAPIKey  string
	EmbedModel   string
	// MatchCandidateK bounds the retrieval candidates per topic.
	MatchCandidateK int
	// MinEmbedCollection is the collection size above which embedding
	// retrieval is used (smaller collections match against the full list).
	MinEmbedCollection int
	// AnalyseMode selects the analysis pipeline: "staged" (default, two
	// calls: extraction then matching) or "unified" (experimental, a single
	// call producing topics with their matches).
	AnalyseMode string
	// SyncInterval controls how often the server reconciles local mindcache
	// metadata against the meta.json sidecars in the blob bucket, which is
	// what keeps several machines sharing one bucket consistent. Values <= 0
	// fall back to DefaultSyncInterval.
	SyncInterval time.Duration

	// PublicFS optionally overrides the on-disk public/ directory for
	// static content served at the site root (e.g. an embedded SPA).
	// When nil, the server falls back to the public/ directory in the
	// current working directory when it exists.
	PublicFS fs.FS
}

// LoadConfigFromEnv reads configuration from environment variables with
// sensible defaults.
func LoadConfigFromEnv() (*Config, error) {
	env := getEnv("APP_ENV", "production")
	defaultDBPath := "mindcache.db"
	defaultStorageURL := "file://."
	if env == "development" {
		defaultDBPath = "data/mindcache.db"
		defaultStorageURL = "file://./data"
	}

	c := &Config{
		Env:                env,
		Port:               getEnv("PORT", "9000"),
		DBPath:             getEnv("DB_PATH", defaultDBPath),
		StorageURL:         getEnv("STORAGE_URL", defaultStorageURL),
		LLMBaseURL:         getEnv("LLM_BASE_URL", ""),
		LLMAPIKey:          getEnv("LLM_API_KEY", "local"),
		LLMModel:           getEnv("LLM_MODEL", ""),
		LLMMaxConcurrency:  getEnvInt("LLM_MAX_CONCURRENCY", 1),
		LLMMaxInputChars:   getEnvInt("LLM_MAX_INPUT_CHARS", 100000),
		LLMGatewaySalt:     getEnv("LLM_GATEWAY_SALT", ""),
		LLMDeviceID:        getEnv("LLM_DEVICE_ID", ""),
		EmbedBaseURL:       getEnv("EMBED_BASE_URL", ""),
		EmbedAPIKey:        getEnv("EMBED_API_KEY", ""),
		EmbedModel:         getEnv("EMBED_MODEL", ""),
		MatchCandidateK:    getEnvInt("MATCH_CANDIDATE_K", 5),
		MinEmbedCollection: getEnvInt("EMBED_MIN_COLLECTION", 30),
		AnalyseMode:        getEnv("ANALYSE_MODE", ""),
		SyncInterval:       time.Duration(getEnvInt("SYNC_INTERVAL_SECONDS", int(DefaultSyncInterval/time.Second))) * time.Second,
	}

	if c.IsDev() {
		if c.LLMBaseURL == "" {
			c.LLMBaseURL = "http://127.0.0.1:1234/v1"
		}
		if c.LLMModel == "" {
			c.LLMModel = "google/gemma-4-e2b"
		}
	}

	if c.EmbedBaseURL == "" {
		c.EmbedBaseURL = c.LLMBaseURL
	}
	if c.EmbedAPIKey == "" {
		c.EmbedAPIKey = c.LLMAPIKey
	}

	if c.AnalyseMode == "" {
		c.AnalyseMode = AnalyseModeStaged
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Analysis pipeline modes.
const (
	// AnalyseModeStaged runs two sequential LLM calls: topic extraction,
	// then matching against existing mindcaches. This is the default.
	AnalyseModeStaged = "staged"
	// AnalyseModeUnified runs a single LLM call that produces topics
	// together with their mindcache matches. Experimental.
	AnalyseModeUnified = "unified"
)

func (c *Config) validate() error {
	if c.IsProd() {
		if c.LLMBaseURL == "" {
			return fmt.Errorf("LLM_BASE_URL is required in production")
		}
		if c.LLMModel == "" {
			return fmt.Errorf("LLM_MODEL is required in production")
		}
	}

	if c.LLMBaseURL != "" && !strings.HasPrefix(c.LLMBaseURL, "http://") && !strings.HasPrefix(c.LLMBaseURL, "https://") {
		return fmt.Errorf("LLM_BASE_URL must start with http:// or https://")
	}

	switch c.AnalyseMode {
	case "", AnalyseModeStaged, AnalyseModeUnified: // empty means staged
	default:
		return fmt.Errorf("ANALYSE_MODE must be %q or %q", AnalyseModeStaged, AnalyseModeUnified)
	}

	validSchemes := []string{"file://", "s3://", "gs://", "mem://"}
	valid := false
	for _, s := range validSchemes {
		if strings.HasPrefix(c.StorageURL, s) {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("STORAGE_URL must start with file:// s3:// gs:// or mem://")
	}

	return nil
}

// IsDev returns true when running in development mode.
func (c *Config) IsDev() bool {
	return c.Env == "development"
}

// IsProd returns true when running in production mode.
func (c *Config) IsProd() bool {
	return c.Env == "production"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
