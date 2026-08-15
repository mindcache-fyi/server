package server

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

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
		Env:               env,
		Port:              getEnv("PORT", "9000"),
		DBPath:            getEnv("DB_PATH", defaultDBPath),
		StorageURL:        getEnv("STORAGE_URL", defaultStorageURL),
		LLMBaseURL:        getEnv("LLM_BASE_URL", ""),
		LLMAPIKey:         getEnv("LLM_API_KEY", "local"),
		LLMModel:          getEnv("LLM_MODEL", ""),
		LLMMaxConcurrency: getEnvInt("LLM_MAX_CONCURRENCY", 1),
	}

	if c.IsDev() {
		if c.LLMBaseURL == "" {
			c.LLMBaseURL = "http://127.0.0.1:1234/v1"
		}
		if c.LLMModel == "" {
			c.LLMModel = "google/gemma-4-e2b"
		}
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

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
