package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

Rules:
- Extract at most 10 topics; merge duplicates so the list is as short as the content allows.
- No two topics may cover the same subject.
- For each topic give a short title (up to 40 characters) and a self-contained, descriptive brief (up to 80 characters).
- Write titles and briefs in the main language of the conversation.
- Return ONLY a valid JSON object — no markdown fences, no text outside the JSON.

Respond with a JSON object in exactly this format:
{ "topics": [ { "title": "...", "brief": "..." } ] }`

// extractTopicsMessagesAddendum extends the extraction prompt when the chat
// carries structured messages.
const extractTopicsMessagesAddendum = `
The conversation is given as numbered messages: [i] role: text. For each topic, also list in "messages" the numbers of all messages that support it — at most 10, and only numbers that appear in the conversation.

The JSON format becomes:
{ "topics": [ { "title": "...", "brief": "...", "messages": [i, ...] } ] }`

const matchMindcachesSystemPrompt = `You are a relevance matching specialist. You receive a numbered list of new topics and a numbered list of existing mindcaches (knowledge notes). For each topic, pick the 0-3 most relevant mindcaches.

Relevance means the same subject: a reader would expect to find both in the same note. Judge by meaning, not by shared wording, and match across languages when the subjects are the same.

Return ONLY a valid JSON object — no markdown fences, no text outside the JSON. Keys are topic numbers as strings; values are arrays of mindcache numbers. Use only numbers that appear in the lists you were given.`

const matchMindcachesUserPrompt = `TOPICS:
%s

MINDCACHES:
%s

Respond with a JSON object in exactly this format:
{ "matches": { "<topic number>": [<mindcache number>, ...], ... } }`

// AnalyseResult holds the output of analysing a chat.
type AnalyseResult struct {
	Topics            []model.Topic
	TopicMindcacheMap map[string][]string
	Mindcaches        []model.Mindcache
}

// AnalyseService extracts topics from chats and matches them to mindcaches.
type AnalyseService struct {
	kv            *cache.KVCache
	repo          *store.MindcacheRepo
	llm           LLM
	dedup         *dedup.Deduplicator[*AnalyseResult]
	maxInputChars int
}

// NewAnalyseService creates an AnalyseService. maxInputChars caps the
// conversation text sent to the LLM per call; values <= 0 disable the cap.
func NewAnalyseService(kv *cache.KVCache, repo *store.MindcacheRepo, llm LLM, maxInputChars int) *AnalyseService {
	return &AnalyseService{
		kv:            kv,
		repo:          repo,
		llm:           llm,
		dedup:         dedup.NewDeduplicator[*AnalyseResult](30 * time.Minute),
		maxInputChars: maxInputChars,
	}
}

// Analyse extracts topics for a chat, deduplicating concurrent and repeated calls.
func (s *AnalyseService) Analyse(ctx context.Context, chat model.Chat) (*AnalyseResult, error) {
	key := contentHash(chat.Content)
	result, err := s.dedup.Do(ctx, key, func(ctx context.Context) (*AnalyseResult, error) {
		slog.Info("analysing (cache miss)", "content_hash", key[:8], "chat", chat.ChatID)
		return s.doAnalyse(ctx, chat)
	})
	if err != nil {
		slog.Error("analyse failed",
			"chat", chat.ChatID,
			"content_hash", key[:8],
			"error", err,
		)
		return nil, err
	}
	return result, nil
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
		Title    string `json:"title"`
		Brief    string `json:"brief"`
		Messages []int  `json:"messages"`
	} `json:"topics"`
}

func (s *AnalyseService) extractTopics(ctx context.Context, chat model.Chat) ([]model.Topic, error) {
	title := chat.Title
	if title == "" {
		title = "(untitled)"
	}
	conversation, indexMap := formatConversation(chat, s.maxInputChars)
	userMessage := fmt.Sprintf("Title: %s\n\nConversation:\n%s", title, conversation)

	systemPrompt := extractTopicsSystemPrompt
	if len(chat.Messages) > 0 {
		systemPrompt += extractTopicsMessagesAddendum
	}

	var parsed topicsResponse
	if err := generateJSONWithRetry(ctx, s.llm, userMessage, systemPrompt, &parsed); err != nil {
		return nil, fmt.Errorf("%w: topics: %w", ErrLLMResponse, err)
	}
	if parsed.Topics == nil {
		return nil, fmt.Errorf("%w: topics: unexpected format", ErrLLMResponse)
	}

	topics := make([]model.Topic, 0, len(parsed.Topics))
	for i, t := range parsed.Topics {
		topic := model.Topic{
			TopicID:    generateTopicID(i),
			Title:      truncateRunes(t.Title, 40),
			Brief:      composeBrief(t.Title, t.Brief),
			SourceChat: chat.ChatID,
		}
		if len(chat.Messages) > 0 && indexMap != nil {
			topic.MessageRefs, topic.SourceExcerpts = buildExcerpts(t.Messages, chat.Messages, indexMap)
		}
		topics = append(topics, topic)
	}
	return topics, nil
}

// formatConversation renders the conversation for the extraction prompt.
// Structured messages become numbered "[i] role: text" lines and the second
// return value maps each rendered line number to its 1-based original
// message index (0 for synthetic lines). Flat content is used as-is with a
// nil map. Input is capped at maxRunes runes.
func formatConversation(chat model.Chat, maxRunes int) (string, []int) {
	if len(chat.Messages) == 0 {
		return truncateConversation(chat.Content, maxRunes), nil
	}
	msgs, indexMap := truncateMessages(chat.Messages, maxRunes)
	var b strings.Builder
	for i, m := range msgs {
		line := m.Content
		if m.Role != "" {
			line = m.Role + ": " + m.Content
		}
		fmt.Fprintf(&b, "[%d] %s", i+1, line)
		if i < len(msgs)-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String(), indexMap
}

// messageCost estimates the rendered size of a numbered message line in runes.
func messageCost(m model.Message) int {
	return len([]rune(m.Content)) + len([]rune(m.Role)) + 8
}

// truncateMessages keeps the head and tail messages intact and inserts a
// marker message when the total text exceeds maxRunes runes. It returns the
// rendered messages plus the 1-based original index of each (0 for the
// marker). maxRunes <= 0 disables truncation.
func truncateMessages(msgs []model.Message, maxRunes int) ([]model.Message, []int) {
	if maxRunes <= 0 || len(msgs) == 0 {
		indexMap := make([]int, len(msgs))
		for i := range msgs {
			indexMap[i] = i + 1
		}
		return msgs, indexMap
	}

	total := 0
	for _, m := range msgs {
		total += messageCost(m)
	}
	if total <= maxRunes {
		indexMap := make([]int, len(msgs))
		for i := range msgs {
			indexMap[i] = i + 1
		}
		return msgs, indexMap
	}

	headBudget := maxRunes * 2 / 3
	tailBudget := maxRunes - headBudget

	headIdxs := make([]int, 0, len(msgs))
	forcedFirst := false
	used := 0
	i := 0
	for ; i < len(msgs); i++ {
		cost := messageCost(msgs[i])
		if used+cost > headBudget {
			if len(headIdxs) == 0 {
				// Always keep at least the first message, trimmed to budget.
				headIdxs = append(headIdxs, i+1)
				forcedFirst = true
				i++
			}
			break
		}
		headIdxs = append(headIdxs, i+1)
		used += cost
	}

	tailIdxs := []int{}
	used = 0
	for j := len(msgs) - 1; j >= i; j-- {
		cost := messageCost(msgs[j])
		if used+cost > tailBudget {
			break
		}
		tailIdxs = append([]int{j + 1}, tailIdxs...)
		used += cost
	}

	if i >= len(msgs) || len(headIdxs)+len(tailIdxs) >= len(msgs) {
		// Head and tail already cover every message.
		all := append(headIdxs, tailIdxs...)
		out := make([]model.Message, len(all))
		for k, idx := range all {
			out[k] = msgs[idx-1]
		}
		return out, all
	}

	out := make([]model.Message, 0, len(headIdxs)+len(tailIdxs)+1)
	indexMap := make([]int, 0, len(headIdxs)+len(tailIdxs)+1)
	for _, idx := range headIdxs {
		m := msgs[idx-1]
		if forcedFirst && idx == headIdxs[0] {
			m.Content = truncateRunes(m.Content, headBudget)
		}
		out = append(out, m)
		indexMap = append(indexMap, idx)
	}
	out = append(out, model.Message{Content: "[... middle of conversation truncated ...]"})
	indexMap = append(indexMap, 0)
	for _, idx := range tailIdxs {
		out = append(out, msgs[idx-1])
		indexMap = append(indexMap, idx)
	}
	return out, indexMap
}

// buildExcerpts validates LLM-reported message indices against the rendered
// index map and turns the referenced messages into "[role] content"
// excerpts. Invalid, synthetic, and duplicate indices are dropped; at most
// 10 excerpts are kept.
func buildExcerpts(reported []int, msgs []model.Message, indexMap []int) ([]int, []string) {
	seen := make(map[int]bool, len(reported))
	refs := make([]int, 0, len(reported))
	excerpts := make([]string, 0, len(reported))
	for _, idx := range reported {
		if idx < 1 || idx > len(indexMap) {
			continue
		}
		orig := indexMap[idx-1]
		if orig == 0 || orig > len(msgs) || seen[orig] {
			continue
		}
		seen[orig] = true
		refs = append(refs, orig)
		excerpts = append(excerpts, messageExcerpt(msgs[orig-1]))
		if len(refs) == 10 {
			break
		}
	}
	return refs, excerpts
}

func messageExcerpt(m model.Message) string {
	text := truncateRunes(m.Content, 1500)
	if m.Role == "" {
		return text
	}
	return m.Role + ": " + text
}

type matchesResponse struct {
	Matches map[string][]int `json:"matches"`
}

func (s *AnalyseService) matchMindcaches(ctx context.Context, topics []model.Topic, mindcaches []model.Mindcache) (map[string][]string, error) {
	if len(topics) == 0 || len(mindcaches) == 0 {
		return map[string][]string{}, nil
	}

	topicLines := make([]string, 0, len(topics))
	for i, t := range topics {
		topicLines = append(topicLines, fmt.Sprintf("%d. %s", i+1, t.Brief))
	}
	mcLines := make([]string, 0, len(mindcaches))
	for i, mc := range mindcaches {
		mcLines = append(mcLines, fmt.Sprintf("%d. %s", i+1, mc.Brief))
	}

	userMessage := fmt.Sprintf(matchMindcachesUserPrompt, strings.Join(topicLines, "\n"), strings.Join(mcLines, "\n"))

	var parsed matchesResponse
	if err := generateJSONWithRetry(ctx, s.llm, userMessage, matchMindcachesSystemPrompt, &parsed); err != nil {
		return nil, fmt.Errorf("%w: matches: %w", ErrLLMResponse, err)
	}

	matches := make(map[string][]string, len(parsed.Matches))
	for topicIdxStr, mcIdxs := range parsed.Matches {
		topicIdx, err := strconv.Atoi(topicIdxStr)
		if err != nil || topicIdx < 1 || topicIdx > len(topics) {
			continue
		}
		topicID := topics[topicIdx-1].TopicID
		seen := make(map[string]bool, len(mcIdxs))
		ids := make([]string, 0, len(mcIdxs))
		for _, mcIdx := range mcIdxs {
			if mcIdx < 1 || mcIdx > len(mindcaches) {
				continue
			}
			mcID := mindcaches[mcIdx-1].ID
			if seen[mcID] {
				continue
			}
			seen[mcID] = true
			ids = append(ids, mcID)
			if len(ids) == 3 {
				break
			}
		}
		if len(ids) > 0 {
			matches[topicID] = ids
		}
	}
	return matches, nil
}

// parseLLMJSONInto unmarshals an LLM reply into out, tolerating markdown
// fences and surrounding chatter.
func parseLLMJSONInto(raw string, out any) error {
	cleaned := stripMarkdownFences(raw)

	if err := json.Unmarshal([]byte(cleaned), out); err == nil {
		return nil
	}

	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(cleaned[start:end+1]), out); err == nil {
			return nil
		}
	}

	return errors.New("failed to parse LLM response as JSON")
}

// generateJSONWithRetry asks the LLM for a JSON object and retries once with
// parse-error feedback when the first reply is not valid JSON.
func generateJSONWithRetry(ctx context.Context, llm LLM, userMessage, systemPrompt string, out any) error {
	raw, err := llm.Generate(ctx, userMessage, systemPrompt)
	if err != nil {
		return err
	}
	if parseErr := parseLLMJSONInto(raw, out); parseErr == nil {
		return nil
	} else {
		retryMessage := userMessage +
			"\n\nYour previous reply was not valid JSON (" + parseErr.Error() +
			"). Reply again with ONLY the JSON object — no markdown fences, no commentary."
		raw, err = llm.Generate(ctx, retryMessage, systemPrompt)
		if err != nil {
			return err
		}
		return parseLLMJSONInto(raw, out)
	}
}

const truncationMarker = "\n\n[... middle of conversation truncated ...]\n\n"

// truncateConversation limits content to roughly maxRunes runes, keeping the
// beginning and the end. maxRunes <= 0 disables truncation.
func truncateConversation(content string, maxRunes int) string {
	r := []rune(content)
	if maxRunes <= 0 || len(r) <= maxRunes {
		return content
	}
	headLen := maxRunes * 2 / 3
	tailLen := maxRunes - headLen
	return string(r[:headLen]) + truncationMarker + string(r[len(r)-tailLen:])
}

// truncateRunes shortens s to at most max runes. max <= 0 disables truncation.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max])
}

// composeBrief builds the display brief from a topic title and summary,
// keeping the "Title: summary" convention used across the product.
func composeBrief(title, summary string) string {
	title = strings.TrimSpace(truncateRunes(title, 40))
	summary = strings.TrimSpace(truncateRunes(summary, 80))
	switch {
	case title == "" && summary == "":
		return ""
	case title == "":
		return summary
	case summary == "":
		return title
	default:
		return truncateRunes(title+": "+summary, 120)
	}
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
