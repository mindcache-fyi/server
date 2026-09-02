package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mindcache-fyi/server/internal/cache"
	"github.com/mindcache-fyi/server/internal/dedup"
	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
	"github.com/rs/xid"
)

const extractInstructions = `You are a knowledge extraction specialist. Given a conversation and a specific topic, extract only the content that is directly relevant to that topic.

Extract the information that is directly related to the topic above. Remove any content that is unrelated, including greetings, small talk, and off-topic discussions. Format the extracted information as clean, well-structured markdown. Include code snippets, technical details, and key insights relevant to the topic. Preserve the factual accuracy.

If the conversation alternates between user and assistant, present the extracted information as a coherent summary organized by subtopic, not as a back-and-forth transcript.

Return ONLY the extracted markdown content with no additional commentary.`

const integrateInstructions = `You are a knowledge integration specialist. Given an existing mindcache document, a conversation, and a specific topic, integrate the new information into the existing document.

Your task:
- Read the existing mindcache document and understand its current structure and content.
- Read the conversation and the specified topic.
- Extract the information from the conversation that is relevant to the topic.
- Integrate this new information into the existing document — do NOT simply append. Merge related content under appropriate headings, add new subsections where needed, and reorganize if it improves clarity.
- Preserve all existing information. Only add or restructure, never remove.
- Maintain clean, well-structured markdown.
- If the topic is already covered in the existing document, enrich it with the new details rather than duplicating.

Return a JSON object with two fields:
- "brief": a leading summary (≤20 characters) capturing the overarching theme, followed by a comma-separated list of every top-level topic as short keyword phrases — like the original topic briefs. Total ≤300 characters. NO sub-points, NO drill-down detail, NO introductory wording like "This document describes". Example: "Database performance: query optimization, connection pooling, vacuum strategies, index selection"
- "content": the full updated markdown document.

Return ONLY valid JSON with no additional commentary or markdown fences.`

// MindcacheService manages mindcache CRUD and LLM-driven content integration.
type MindcacheService struct {
	repo          *store.MindcacheRepo
	storage       *Storage
	llm           LLM
	kv            *cache.KVCache
	createDedup   *dedup.Deduplicator[model.Mindcache]
	updateDedup   *dedup.Deduplicator[bool]
	maxInputChars int
	embedder      EmbeddingsProvider
	searchIdx     *SearchIndexService
}

// NewMindcacheService creates a MindcacheService. maxInputChars caps the
// conversation text sent to the LLM per call; values <= 0 disable the cap.
// embedder and searchIdx may be nil — briefs are then never embedded, and
// content is never full-text indexed.
func NewMindcacheService(repo *store.MindcacheRepo, storage *Storage, llm LLM, kv *cache.KVCache, maxInputChars int, embedder EmbeddingsProvider, searchIdx *SearchIndexService) *MindcacheService {
	return &MindcacheService{
		repo:          repo,
		storage:       storage,
		llm:           llm,
		kv:            kv,
		createDedup:   dedup.NewDeduplicator[model.Mindcache](30 * time.Minute),
		updateDedup:   dedup.NewDeduplicator[bool](30 * time.Minute),
		maxInputChars: maxInputChars,
		embedder:      embedder,
		searchIdx:     searchIdx,
	}
}

// List returns all mindcaches ordered by most recently updated.
func (s *MindcacheService) List(ctx context.Context) ([]model.Mindcache, error) {
	return s.repo.List(ctx)
}

// GetByID returns a mindcache and its main content. It returns nil mindcache and
// empty content when the mindcache does not exist.
func (s *MindcacheService) GetByID(ctx context.Context, id string) (*model.Mindcache, string, error) {
	mc, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if mc == nil {
		return nil, "", nil
	}

	mainContent := ""
	if data, err := s.storage.Read(ctx, model.MindcacheMainPath(id)); err == nil {
		mainContent = string(data)
	}
	return mc, mainContent, nil
}

// CreateFromTopic creates a mindcache from a cached topic, deduplicating calls.
func (s *MindcacheService) CreateFromTopic(ctx context.Context, topicId string) (*model.Mindcache, error) {
	if cached, ok := s.createDedup.Get(topicId); ok {
		row, err := s.repo.GetByID(ctx, cached.ID)
		if err == nil && row != nil {
			return row, nil
		}
		s.createDedup.Invalidate(topicId)
	}

	result, err := s.createDedup.Do(ctx, topicId, func(ctx context.Context) (model.Mindcache, error) {
		return s.doCreateFromTopic(ctx, topicId)
	})
	if err != nil {
		slog.Error("create mindcache failed", "topic", topicId, "error", err)
		return nil, err
	}
	return &result, nil
}

func (s *MindcacheService) doCreateFromTopic(ctx context.Context, topicId string) (model.Mindcache, error) {
	topicRaw, ok := s.kv.Get(topicKey(topicId))
	if !ok {
		return model.Mindcache{}, model.ErrTopicNotFound
	}
	var topic model.Topic
	if err := json.Unmarshal([]byte(topicRaw), &topic); err != nil {
		return model.Mindcache{}, err
	}

	chatRaw, ok := s.kv.Get(chatKey(topic.SourceChat))
	if !ok {
		return model.Mindcache{}, model.ErrChatNotFound
	}
	var chat model.Chat
	if err := json.Unmarshal([]byte(chatRaw), &chat); err != nil {
		return model.Mindcache{}, err
	}

	mainContent, err := s.extractTopicContent(ctx, topic.Brief, chat)
	if err != nil {
		return model.Mindcache{}, err
	}

	id := xid.New().String()
	now := time.Now().UTC()
	sourceURLs := []string{chat.SourceURL}

	// Content is written before the database row so the metadata reconciler
	// never sees a row without bucket objects (and deletes it), and the
	// meta.json sidecar — the cross-machine source of truth for metadata —
	// carries exactly the timestamps stored in the row.
	if err := s.storage.Write(ctx, model.MindcacheMainPath(id), []byte(mainContent)); err != nil {
		return model.Mindcache{}, err
	}

	mc, err := s.repo.CreateWithID(ctx, id, topic.Brief, sourceURLs, now, now)
	if err != nil {
		return model.Mindcache{}, err
	}

	if err := WriteMeta(ctx, s.storage, mc); err != nil {
		// The row and content already exist; a missing sidecar only delays
		// cross-machine visibility, so log and let the reconciler retry.
		slog.Warn("write meta.json failed", "mindcache", id, "error", err)
	} else {
		markMetaApplied(ctx, s.repo, s.storage, id)
	}

	s.indexMindcache(ctx, mc.ID)

	embedBrief(ctx, s.embedder, s.repo, mc.ID, mc.Brief)

	return *mc, nil
}

// Update integrates a topic's source chat into an existing mindcache.
func (s *MindcacheService) Update(ctx context.Context, mindcacheId, topicId string) (bool, error) {
	cacheKey := mindcacheId + "_" + topicId
	if _, ok := s.updateDedup.Get(cacheKey); ok {
		row, err := s.repo.GetByID(ctx, mindcacheId)
		if err == nil && row != nil {
			return true, nil
		}
		s.updateDedup.Invalidate(cacheKey)
	}

	ok, err := s.updateDedup.Do(ctx, cacheKey, func(ctx context.Context) (bool, error) {
		return s.doUpdate(ctx, mindcacheId, topicId)
	})
	if err != nil {
		slog.Error("update mindcache failed", "mindcache", mindcacheId, "topic", topicId, "error", err)
		return false, err
	}
	return ok, nil
}

type integrateResponse struct {
	Brief   string `json:"brief"`
	Content string `json:"content"`
}

func (s *MindcacheService) doUpdate(ctx context.Context, mindcacheId, topicId string) (bool, error) {
	mc, err := s.repo.GetByID(ctx, mindcacheId)
	if err != nil {
		return false, err
	}
	if mc == nil {
		return false, nil
	}

	topicRaw, ok := s.kv.Get(topicKey(topicId))
	if !ok {
		return false, model.ErrTopicNotFound
	}
	var topic model.Topic
	if err := json.Unmarshal([]byte(topicRaw), &topic); err != nil {
		return false, err
	}

	chatRaw, ok := s.kv.Get(chatKey(topic.SourceChat))
	if !ok {
		return false, model.ErrChatNotFound
	}
	var chat model.Chat
	if err := json.Unmarshal([]byte(chatRaw), &chat); err != nil {
		return false, err
	}

	existing := ""
	if data, err := s.storage.Read(ctx, model.MindcacheMainPath(mindcacheId)); err == nil {
		existing = string(data)
	}

	userMessage := fmt.Sprintf(
		"Topic: %s\n\nNew conversation (extract relevant info and integrate into the existing document):\n%s\n\nExisting mindcache document:\n%s",
		topic.Brief, truncateConversation(chat.Content, s.maxInputChars), existing,
	)

	var integrated integrateResponse
	if err := generateJSONWithRetry(ctx, s.llm, userMessage, integrateInstructions, &integrated); err != nil {
		return false, fmt.Errorf("%w: integrate: %w", ErrLLMResponse, err)
	}

	if err := s.storage.Write(ctx, model.MindcacheMainPath(mindcacheId), []byte(integrated.Content)); err != nil {
		return false, err
	}

	now := time.Now().UTC()
	sourceUrls := appendSourceURL(mc.SourceURLs, chat.SourceURL)
	if err := s.repo.UpdateWithTime(ctx, mindcacheId, integrated.Brief, sourceUrls, now); err != nil {
		return false, err
	}

	updated := *mc
	updated.Brief = integrated.Brief
	updated.SourceURLs = sourceUrls
	updated.UpdatedAt = now
	if err := WriteMeta(ctx, s.storage, &updated); err != nil {
		// The row and content already hold the change; a missing sidecar
		// only delays cross-machine visibility, so let the reconciler retry.
		slog.Warn("write meta.json failed", "mindcache", mindcacheId, "error", err)
	} else {
		markMetaApplied(ctx, s.repo, s.storage, mindcacheId)
	}

	s.indexMindcache(ctx, mindcacheId)

	embedBrief(ctx, s.embedder, s.repo, mindcacheId, integrated.Brief)

	return true, nil
}

// Delete removes a mindcache and its stored content. It returns false when the
// mindcache does not exist.
func (s *MindcacheService) Delete(ctx context.Context, id string) (bool, error) {
	mc, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return false, err
	}
	if mc == nil {
		return false, nil
	}

	if err := s.repo.Delete(ctx, id); err != nil {
		return false, err
	}
	_ = s.storage.DeleteDir(ctx, model.MindcachePrefix(id))
	if s.searchIdx != nil {
		s.searchIdx.RemoveFromIndex(ctx, id)
	}
	return true, nil
}

// indexMindcache refreshes the full-text index entry of a mindcache after a
// content write. Failures are logged, never fatal — the startup reconcile
// repairs any drift.
func (s *MindcacheService) indexMindcache(ctx context.Context, id string) {
	if s.searchIdx == nil {
		return
	}
	if err := s.searchIdx.IndexMindcache(ctx, id); err != nil {
		slog.Warn("search index update failed", "mindcache", id, "error", err)
	}
}

func (s *MindcacheService) extractTopicContent(ctx context.Context, topic string, chat model.Chat) (string, error) {
	userMessage := fmt.Sprintf("Topic: %s\n\nConversation:\n%s", topic, truncateConversation(chat.Content, s.maxInputChars))
	return s.llm.Generate(ctx, userMessage, extractInstructions)
}

func appendSourceURL(existing []string, newURL string) []string {
	for _, u := range existing {
		if u == newURL {
			return existing
		}
	}
	return append(existing, newURL)
}
