package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrTopicNotFound     = errors.New("topic not found")
	ErrChatNotFound      = errors.New("chat not found")
	ErrMindcacheNotFound = errors.New("mindcache not found")
	ErrLLMNotConfigured  = errors.New("LLM not configured")
)

// Chat represents a conversation from an AI provider.
type Chat struct {
	// Unique chat identifier
	ChatID string `json:"chatId"`
	// Provider enum: gemini, claude, deepseek, yuanbao, chatgpt
	Provider string `json:"provider" enums:"gemini,claude,deepseek,yuanbao,chatgpt"`
	// Source URL of the conversation
	SourceURL string `json:"sourceUrl"`
	// Chat title
	Title string `json:"title"`
	// Full chat content as plain text
	Content string `json:"content"`
}

// Topic represents an extracted topic from a chat.
type Topic struct {
	// Unique topic identifier
	TopicID string `json:"topicId"`
	// Short topic title
	Title string `json:"title"`
	// Brief summary of the topic
	Brief string `json:"brief"`
	// Source chat ID this topic was extracted from
	SourceChat string `json:"sourceChat"`
}

// Mindcache represents a knowledge cache entry.
type Mindcache struct {
	// Unique mindcache identifier
	ID string `json:"id"`
	// Brief summary
	Brief string `json:"brief"`
	// Creation timestamp
	CreatedAt time.Time `json:"createdAt"`
	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt"`
	// Source conversation URLs
	SourceURLs []string `json:"sourceUrls"`
}

// AnalyseRequest is the request body for the analyse endpoint.
type AnalyseRequest struct {
	// Chat payload from the extension
	Chat Chat `json:"chat"`
}

// AnalyseResponse is the response body for the analyse endpoint.
type AnalyseResponse struct {
	// Extracted topics
	Topics []Topic `json:"topics"`
	// Per-topic matched mindcache IDs (topicId → mindcacheId[])
	TopicMindcacheMap map[string][]string `json:"topicMindcacheMap"`
	// Related (matched) mindcaches
	Mindcaches []Mindcache `json:"mindcaches"`
}

// ClearAnalyseResponse is the response body for clearing analyse results.
type ClearAnalyseResponse struct {
	// Whether cache entries were cleared
	Cleared bool `json:"cleared"`
}

// CreateRequest is the request body for creating a mindcache.
type CreateRequest struct {
	// Source topic ID from /analyse response
	TopicID string `json:"topicId"`
}

// CreateResponse is the response body for creating a mindcache.
type CreateResponse struct {
	Success bool `json:"success"`
	// Created mindcache ID
	MindcacheID string `json:"mindcacheId,omitempty"`
	// Error message when success is false
	Error string `json:"error,omitempty"`
}

// UpdateRequest is the request body for updating a mindcache.
type UpdateRequest struct {
	// Target mindcache ID
	MindcacheID string `json:"mindcacheId"`
	// Source topic ID from /analyse response
	TopicID string `json:"topicId"`
}

// UpdateResponse is the response body for updating a mindcache.
type UpdateResponse struct {
	Success bool `json:"success"`
	// Target mindcache ID
	MindcacheID string `json:"mindcacheId"`
	// Error message when success is false
	Error string `json:"error,omitempty"`
}

// ListResponse is the response body for listing mindcaches.
type ListResponse struct {
	// All mindcaches
	Mindcaches []Mindcache `json:"mindcaches"`
}

// SearchResult is one full-text match with its highlight snippet.
type SearchResult struct {
	// Matched mindcache metadata
	Mindcache Mindcache `json:"mindcache"`
	// Which indexed fields matched: "brief", "content", or both
	MatchedIn []string `json:"matchedIn"`
	// Text window around the first content match; matches are wrapped in
	// \x01/\x02 marker characters for client-side highlighting
	Snippet string `json:"snippet"`
	// Relevance score; higher is better
	Score float64 `json:"score"`
}

// SearchResponse is the response body for full-text search.
type SearchResponse struct {
	// Ranked matches
	Results []SearchResult `json:"results"`
}

// GetResponse is the response body for getting a mindcache.
type GetResponse struct {
	Mindcache Mindcache `json:"mindcache"`
	// Main markdown content
	MainContent string `json:"mainContent"`
}

// ErrorResponse is the standard error response body.
type ErrorResponse struct {
	// Error description
	Error string `json:"error"`
}

// MindcacheMainPath returns the storage path for a mindcache's main content.
func MindcacheMainPath(id string) string {
	return "mindcache/" + id + "/main.md"
}

// MindcacheMetaPath returns the storage path for a mindcache's meta.json
// sidecar, the cross-machine source of truth for its metadata.
func MindcacheMetaPath(id string) string {
	return "mindcache/" + id + "/meta.json"
}

// MetaSuffix and MainSuffix are the per-mindcache object suffixes inside a
// bucket listing.
const (
	MetaSuffix = "/meta.json"
	MainSuffix = "/main.md"
)

// MetaSchema is the current meta.json schema version.
const MetaSchema = 1

// MetaFile is the JSON sidecar stored next to a mindcache's main content. It
// carries the metadata fields that multiple machines must agree on, so the
// blob bucket — not any single local database — is the source of truth.
type MetaFile struct {
	Schema     int       `json:"schema"`
	Brief      string    `json:"brief"`
	SourceURLs []string  `json:"sourceUrls"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// MindcachePrefix returns the storage prefix for a mindcache.
func MindcachePrefix(id string) string {
	return "mindcache/" + id + "/"
}

// MindcacheRoot is the storage prefix containing every mindcache.
const MindcacheRoot = "mindcache/"

// MindcacheAssetPath returns the storage path for a mindcache asset.
func MindcacheAssetPath(id, filename string) string {
	return "mindcache/" + id + "/assets/" + filename
}

var validProviders = map[string]bool{
	"gemini":   true,
	"claude":   true,
	"deepseek": true,
	"yuanbao":  true,
	"chatgpt":  true,
}

// ValidateChat validates a chat before processing.
func ValidateChat(chat *Chat) error {
	if chat.ChatID == "" {
		return fmt.Errorf("chatId is required")
	}
	if chat.Content == "" {
		return fmt.Errorf("content is required")
	}
	if !validProviders[chat.Provider] {
		return fmt.Errorf("provider must be one of: gemini, claude, deepseek, yuanbao, chatgpt")
	}
	return nil
}

// ValidateID validates an identifier for safe use in paths.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if id == ".." {
		return fmt.Errorf("invalid id")
	}
	if strings.ContainsAny(id, "/\\.") {
		return fmt.Errorf("id contains invalid characters")
	}
	return nil
}
