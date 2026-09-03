package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/compat"
)

// LLM abstracts a language model client capable of generating text.
type LLM interface {
	Generate(ctx context.Context, userMessage string, systemPrompt string) (string, error)
	IsConfigured() bool
	GetConfig() LLMConfig
}

// LLMConfig describes the runtime configuration of an LLM client.
type LLMConfig struct {
	Configured     bool   `json:"configured"`
	Model          string `json:"model"`
	BaseURL        string `json:"baseUrl"`
	MaxConcurrency int    `json:"maxConcurrency"`
}

// Semaphore bounds concurrency using a buffered channel of permits.
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore creates a Semaphore that allows up to max concurrent holders.
func NewSemaphore(max int) *Semaphore {
	if max < 1 {
		max = 1
	}
	return &Semaphore{ch: make(chan struct{}, max)}
}

// Acquire blocks until a permit is available or ctx is cancelled.
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a previously acquired permit.
func (s *Semaphore) Release() {
	<-s.ch
}

// LLMClient is an LLM backed by an OpenAI-compatible endpoint via the goai SDK.
type LLMClient struct {
	model          provider.LanguageModel
	sem            *Semaphore
	modelName      string
	baseURL        string
	maxConcurrency int
}

type loggingTransport struct {
	next http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyStr string
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		bodyStr = prettyJSON(body)
	}
	fmt.Printf("\n====== [🚀 LLM Request] ======\nURL: %s\nBody: %s\n==============================\n\n",
		req.URL.String(), bodyStr)

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		fmt.Printf("\n====== [❌ LLM Error] ======\n%v\n==============================\n\n", err)
		return nil, err
	}

	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	fmt.Printf("\n====== [📥 LLM Response] ======\nStatus: %d\nBody: %s\n==============================\n\n",
		resp.StatusCode, prettyJSON(respBody))

	return resp, nil
}

func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil {
		return buf.String()
	}
	return string(data)
}

// NewLLMClient creates an LLMClient for the given OpenAI-compatible endpoint.
// When verbose is true, full request/response bodies are logged (development
// only). A non-nil signer adds HMAC gateway headers to every request; nil
// keeps plain BYOK behaviour.
func NewLLMClient(baseURL, apiKey, model string, maxConcurrency int, verbose bool, signer *RequestSigner) (*LLMClient, error) {
	if baseURL == "" {
		return nil, errors.New("llm: baseURL is required")
	}
	if model == "" {
		return nil, errors.New("llm: model is required")
	}

	var transport http.RoundTripper = http.DefaultTransport
	if verbose {
		transport = &loggingTransport{next: transport}
	}
	transport = signer.Transport(transport)

	var httpClient *http.Client
	if verbose || signer != nil {
		httpClient = &http.Client{Transport: transport}
	}

	opts := []compat.Option{
		compat.WithBaseURL(baseURL),
		compat.WithAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, compat.WithHTTPClient(httpClient))
	}

	m := compat.Chat(model, opts...)

	return &LLMClient{
		model:          m,
		sem:            NewSemaphore(maxConcurrency),
		modelName:      model,
		baseURL:        baseURL,
		maxConcurrency: maxConcurrency,
	}, nil
}

// Generate produces text for the given user message and optional system prompt.
func (c *LLMClient) Generate(ctx context.Context, userMessage string, systemPrompt string) (string, error) {
	if err := c.sem.Acquire(ctx); err != nil {
		return "", err
	}
	defer c.sem.Release()

	opts := []goai.Option{
		goai.WithPrompt(userMessage),
		goai.WithTemperature(0.3),
		goai.WithMaxRetries(0),
	}
	if systemPrompt != "" {
		opts = append(opts, goai.WithSystem(systemPrompt))
	}

	result, err := goai.GenerateText(ctx, c.model, opts...)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrLLMUpstream, err)
	}
	return result.Text, nil
}

// IsConfigured reports whether the client is configured. The client is always
// configured at construction, so this returns true.
func (c *LLMClient) IsConfigured() bool {
	return true
}

// GetConfig returns the client's configuration.
func (c *LLMClient) GetConfig() LLMConfig {
	return LLMConfig{
		Configured:     true,
		Model:          c.modelName,
		BaseURL:        c.baseURL,
		MaxConcurrency: c.maxConcurrency,
	}
}
