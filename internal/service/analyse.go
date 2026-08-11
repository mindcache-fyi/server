package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mindcache-fyi/server/internal/cache"
	"github.com/mindcache-fyi/server/internal/dedup"
	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
)

const extractTopicsSystemPrompt = `You are a topic extraction specialist. Analyse the given conversation and extract the main topics discussed.

For each topic, provide a concise brief summary (max 120 characters) with a leading title followed by ":"

Rules:
- Identify at most 10 distinct topics from the conversation. The topics should be as most less as possible.
- Each brief should be self-contained and descriptive, summarized in the main language of the conversation.
- No same keywords should be shared between briefs
- Return ONLY valid JSON with no markdown formatting

Respond with a JSON object in this exact format:
{ "topics": [{ "brief": "..." }] }`

const matchMindcachesPrompt = `You are a relevance matching specialist. For each topic below, identify the top 0-3 most relevant mindcaches.

TOPICS:
%s

MINDCACHES:
%s

Return ONLY valid JSON — no markdown, no extra text:
{ "matches": { "<topicId>": ["<mcId>", ...], ... } }

For each topic return 0-3 IDs (the number of matching mindcaches determines how many). Only use IDs that exist in the MINDCACHES list above.`

// AnalyseResult holds the output of analysing a chat.
type AnalyseResult struct {
	Topics            []model.Topic
	TopicMindcacheMap map[string][]string
	Mindcaches        []model.Mindcache
}

// AnalyseService extracts topics from chats and matches them to mindcaches.
type AnalyseService struct {
	kv    *cache.KVCache
	repo  *store.MindcacheRepo
	llm   LLM
	dedup *dedup.Deduplicator[*AnalyseResult]
}

// NewAnalyseService creates an AnalyseService.
func NewAnalyseService(kv *cache.KVCache, repo *store.MindcacheRepo, llm LLM) *AnalyseService {
	return &AnalyseService{
		kv:    kv,
		repo:  repo,
		llm:   llm,
		dedup: dedup.NewDeduplicator[*AnalyseResult](30 * time.Minute),
	}
}

// Analyse extracts topics for a chat, deduplicating concurrent and repeated calls.
func (s *AnalyseService) Analyse(ctx context.Context, chat model.Chat) (*AnalyseResult, error) {
	key := contentHash(chat.Content)
	return s.dedup.Do(ctx, key, func(ctx context.Context) (*AnalyseResult, error) {
		slog.Info("analysing (cache miss)", "content_hash", key[:8], "chat", chat.ChatID)
		return s.doAnalyse(ctx, chat)
	})
}

// ClearCache drops cached analysis results and their KV entries for a chat.
func (s *AnalyseService) ClearCache(ctx context.Context, chat model.Chat) (bool, error) {
	key := contentHash(chat.Content)

	cleared := false
	if result, ok := s.dedup.Get(key); ok && result != nil {
		s.kv.Delete(chatKey(chat.ChatID))
		for _, t := range result.Topics {
			s.kv.Delete(topicKey(t.TopicID))
		}
		cleared = true
	}

	s.dedup.Invalidate(key)
	slog.Info("cleared analyse cache", "content", key[:8], "cleared", cleared)
	return cleared, nil
}

func (s *AnalyseService) doAnalyse(ctx context.Context, chat model.Chat) (*AnalyseResult, error) {
	slog.Info("starting analysis", "chat", chat.ChatID)

	chatJSON, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	s.kv.Set(chatKey(chat.ChatID), string(chatJSON))

	topics, err := s.extractTopics(ctx, chat)
	if err != nil {
		return nil, err
	}

	for _, topic := range topics {
		topicJSON, err := json.Marshal(topic)
		if err != nil {
			return nil, err
		}
		s.kv.Set(topicKey(topic.TopicID), string(topicJSON))
	}

	mindcaches, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	topicMindcacheMap, err := s.matchMindcaches(ctx, topics, mindcaches)
	if err != nil {
		return nil, err
	}

	matchedIDs := make(map[string]bool)
	for _, ids := range topicMindcacheMap {
		for _, id := range ids {
			matchedIDs[id] = true
		}
	}
	matched := make([]model.Mindcache, 0, len(matchedIDs))
	for _, mc := range mindcaches {
		if matchedIDs[mc.ID] {
			matched = append(matched, mc)
		}
	}

	return &AnalyseResult{
		Topics:            topics,
		TopicMindcacheMap: topicMindcacheMap,
		Mindcaches:        matched,
	}, nil
}

type topicsResponse struct {
	Topics []struct {
		Brief string `json:"brief"`
	} `json:"topics"`
}

func (s *AnalyseService) extractTopics(ctx context.Context, chat model.Chat) ([]model.Topic, error) {
	title := chat.Title
	if title == "" {
		title = "(untitled)"
	}
	userMessage := fmt.Sprintf("Title: %s\n\nConversation:\n%s", title, chat.Content)

	raw, err := s.llm.Generate(ctx, userMessage, extractTopicsSystemPrompt)
	if err != nil {
		return nil, err
	}

	parsed, err := parseLLMJSON[topicsResponse](raw)
	if err != nil {
		return nil, err
	}
	if parsed.Topics == nil {
		return nil, fmt.Errorf("llm returned unexpected format")
	}

	topics := make([]model.Topic, 0, len(parsed.Topics))
	for i, t := range parsed.Topics {
		brief := t.Brief
		if r := []rune(brief); len(r) > 120 {
			brief = string(r[:120])
		}
		topics = append(topics, model.Topic{
			TopicID:    generateTopicID(i),
			Brief:      brief,
			SourceChat: chat.ChatID,
		})
	}
	return topics, nil
}

type matchesResponse struct {
	Matches map[string][]string `json:"matches"`
}

func (s *AnalyseService) matchMindcaches(ctx context.Context, topics []model.Topic, mindcaches []model.Mindcache) (map[string][]string, error) {
	if len(topics) == 0 || len(mindcaches) == 0 {
		return map[string][]string{}, nil
	}

	topicLines := make([]string, 0, len(topics))
	for _, t := range topics {
		topicLines = append(topicLines, t.TopicID+": "+t.Brief)
	}
	mcLines := make([]string, 0, len(mindcaches))
	for _, mc := range mindcaches {
		mcLines = append(mcLines, mc.ID+": "+mc.Brief)
	}

	prompt := fmt.Sprintf(matchMindcachesPrompt, strings.Join(topicLines, "\n"), strings.Join(mcLines, "\n"))

	raw, err := s.llm.Generate(ctx, prompt, "")
	if err != nil {
		return nil, err
	}

	parsed, err := parseLLMJSON[matchesResponse](raw)
	if err != nil {
		return nil, err
	}
	if parsed.Matches == nil {
		return map[string][]string{}, nil
	}
	return parsed.Matches, nil
}

func parseLLMJSON[T any](raw string) (T, error) {
	var result T
	cleaned := stripMarkdownFences(raw)

	if err := json.Unmarshal([]byte(cleaned), &result); err == nil {
		return result, nil
	}

	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(cleaned[start:end+1]), &result); err == nil {
			return result, nil
		}
	}

	return result, fmt.Errorf("failed to parse LLM response as JSON")
}

func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func generateTopicID(index int) string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 36)
	rand := strings.ReplaceAll(uuid.New().String(), "-", "")[:6]
	return fmt.Sprintf("topic_%s%s_%d", ts, rand, index)
}

func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func chatKey(id string) string {
	return "c_" + id
}

func topicKey(id string) string {
	return "t_" + id
}
