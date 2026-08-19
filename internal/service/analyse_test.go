package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mindcache-fyi/server/internal/model"
)

// scriptedLLM returns canned replies in order and records every call.
type scriptedLLM struct {
	replies    []string
	errs       []error
	calls      int
	lastUser   string
	lastSystem string
}

func (m *scriptedLLM) Generate(_ context.Context, userMessage, systemPrompt string) (string, error) {
	m.lastUser = userMessage
	m.lastSystem = systemPrompt
	i := m.calls
	m.calls++
	if i < len(m.errs) && m.errs[i] != nil {
		return "", m.errs[i]
	}
	if i < len(m.replies) {
		return m.replies[i], nil
	}
	return "", errors.New("no scripted reply")
}

func (m *scriptedLLM) IsConfigured() bool { return true }

func (m *scriptedLLM) GetConfig() LLMConfig { return LLMConfig{} }

func newTestAnalyseService(llm LLM, maxInputChars int) *AnalyseService {
	return NewAnalyseService(nil, nil, llm, maxInputChars)
}

func testChat(content string) model.Chat {
	return model.Chat{
		ChatID:  "chat-1",
		Title:   "Test chat",
		Content: content,
	}
}

func TestExtractTopics_ParsesTitleAndBrief(t *testing.T) {
	llm := &scriptedLLM{replies: []string{
		`{"topics":[{"title":"Docker networking","brief":"bridge vs host modes"}]}`,
	}}
	svc := newTestAnalyseService(llm, 0)

	topics, err := svc.extractTopics(context.Background(), testChat("hello"))
	if err != nil {
		t.Fatalf("extractTopics: %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1", len(topics))
	}
	if topics[0].Title != "Docker networking" {
		t.Errorf("Title = %q, want %q", topics[0].Title, "Docker networking")
	}
	if want := "Docker networking: bridge vs host modes"; topics[0].Brief != want {
		t.Errorf("Brief = %q, want %q", topics[0].Brief, want)
	}
	if llm.calls != 1 {
		t.Errorf("calls = %d, want 1", llm.calls)
	}
}

func TestExtractTopics_TruncatesLongTitlesAndBriefs(t *testing.T) {
	longTitle := strings.Repeat("题", 60)
	longBrief := strings.Repeat("要", 200)
	llm := &scriptedLLM{replies: []string{
		`{"topics":[{"title":"` + longTitle + `","brief":"` + longBrief + `"}]}`,
	}}
	svc := newTestAnalyseService(llm, 0)

	topics, err := svc.extractTopics(context.Background(), testChat("hello"))
	if err != nil {
		t.Fatalf("extractTopics: %v", err)
	}
	if got := len([]rune(topics[0].Title)); got > 40 {
		t.Errorf("Title len = %d runes, want <= 40", got)
	}
	if got := len([]rune(topics[0].Brief)); got > 120 {
		t.Errorf("Brief len = %d runes, want <= 120", got)
	}
}

func TestExtractTopics_RetriesOnInvalidJSON(t *testing.T) {
	llm := &scriptedLLM{replies: []string{
		"Sorry, I cannot help with that.",
		`{"topics":[{"title":"SQLite WAL","brief":"write-ahead logging trade-offs"}]}`,
	}}
	svc := newTestAnalyseService(llm, 0)

	topics, err := svc.extractTopics(context.Background(), testChat("hello"))
	if err != nil {
		t.Fatalf("extractTopics: %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1", len(topics))
	}
	if llm.calls != 2 {
		t.Fatalf("calls = %d, want 2", llm.calls)
	}
	if !strings.Contains(llm.lastUser, "not valid JSON") {
		t.Errorf("retry message does not mention the parse error: %q", llm.lastUser)
	}
}

func TestExtractTopics_FailsAfterRetry(t *testing.T) {
	llm := &scriptedLLM{replies: []string{"nope", "still nope"}}
	svc := newTestAnalyseService(llm, 0)

	_, err := svc.extractTopics(context.Background(), testChat("hello"))
	if err == nil {
		t.Fatal("extractTopics succeeded, want error")
	}
	if !errors.Is(err, ErrLLMResponse) {
		t.Errorf("error = %v, want ErrLLMResponse wrap", err)
	}
	if llm.calls != 2 {
		t.Errorf("calls = %d, want 2", llm.calls)
	}
}

func TestExtractTopics_UpstreamError(t *testing.T) {
	llm := &scriptedLLM{errs: []error{errors.New("boom")}}
	svc := newTestAnalyseService(llm, 0)

	_, err := svc.extractTopics(context.Background(), testChat("hello"))
	if err == nil {
		t.Fatal("extractTopics succeeded, want error")
	}
	if llm.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on upstream error)", llm.calls)
	}
}

func TestExtractTopics_TruncatesLongConversation(t *testing.T) {
	llm := &scriptedLLM{replies: []string{`{"topics":[]}`}}
	svc := newTestAnalyseService(llm, 60)

	content := strings.Repeat("abcdefghij", 20) // 200 runes
	if _, err := svc.extractTopics(context.Background(), testChat(content)); err != nil {
		t.Fatalf("extractTopics: %v", err)
	}
	if !strings.Contains(llm.lastUser, truncationMarker) {
		t.Error("user message does not contain the truncation marker")
	}
	if strings.Contains(llm.lastUser, strings.Repeat("abcdefghij", 20)) {
		t.Error("user message still contains the full conversation")
	}
}

func TestMatchMindcaches_MapsIndicesAndDropsInvalid(t *testing.T) {
	llm := &scriptedLLM{replies: []string{
		`{"matches":{"1":[2,99,2],"2":[1],"99":[1],"x":[1]}}`,
	}}
	svc := newTestAnalyseService(llm, 0)

	topics := []model.Topic{
		{TopicID: "topic-a", Brief: "first"},
		{TopicID: "topic-b", Brief: "second"},
	}
	mindcaches := []model.Mindcache{
		{ID: "mc-1", Brief: "one"},
		{ID: "mc-2", Brief: "two"},
		{ID: "mc-3", Brief: "three"},
	}

	matches, err := svc.matchMindcaches(context.Background(), topics, mindcaches)
	if err != nil {
		t.Fatalf("matchMindcaches: %v", err)
	}
	if got := matches["topic-a"]; len(got) != 1 || got[0] != "mc-2" {
		t.Errorf("matches[topic-a] = %v, want [mc-2] (out-of-range and duplicates dropped)", got)
	}
	if got := matches["topic-b"]; len(got) != 1 || got[0] != "mc-1" {
		t.Errorf("matches[topic-b] = %v, want [mc-1]", got)
	}
	if len(matches) != 2 {
		t.Errorf("len(matches) = %d, want 2 (invalid topic indices dropped)", len(matches))
	}
}

func TestMatchMindcaches_EmptyShortCircuit(t *testing.T) {
	llm := &scriptedLLM{}
	svc := newTestAnalyseService(llm, 0)

	matches, err := svc.matchMindcaches(context.Background(), nil, []model.Mindcache{{ID: "mc-1"}})
	if err != nil {
		t.Fatalf("matchMindcaches: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("len(matches) = %d, want 0", len(matches))
	}
	if llm.calls != 0 {
		t.Errorf("calls = %d, want 0", llm.calls)
	}
}

func TestTruncateConversation(t *testing.T) {
	if got := truncateConversation("short", 100); got != "short" {
		t.Errorf("short content changed: %q", got)
	}
	if got := truncateConversation("anything", 0); got != "anything" {
		t.Errorf("max <= 0 must disable truncation: %q", got)
	}

	content := strings.Repeat("0123456789", 100) // 1000 runes
	got := truncateConversation(content, 120)
	if !strings.Contains(got, truncationMarker) {
		t.Fatal("missing truncation marker")
	}
	if !strings.HasPrefix(got, "01234567890123456789") {
		t.Errorf("head not preserved: %q...", got[:30])
	}
	if !strings.HasSuffix(got, "01234567890123456789") {
		t.Errorf("tail not preserved: ...%q", got[len(got)-30:])
	}
	head := strings.Split(got, truncationMarker)[0]
	tail := strings.Split(got, truncationMarker)[1]
	if len([]rune(head)) != 80 || len([]rune(tail)) != 40 {
		t.Errorf("head/tail rune counts = %d/%d, want 80/40", len([]rune(head)), len([]rune(tail)))
	}
}

func TestParseLLMJSONInto(t *testing.T) {
	var out topicsResponse

	if err := parseLLMJSONInto("```json\n{\"topics\":[]}\n```", &out); err != nil {
		t.Errorf("fenced JSON: %v", err)
	}
	if err := parseLLMJSONInto("Sure! Here you go: {\"topics\":[]} hope that helps", &out); err != nil {
		t.Errorf("JSON with chatter: %v", err)
	}
	if err := parseLLMJSONInto("no json here at all", &out); err == nil {
		t.Error("invalid input parsed without error")
	}
}

func TestGenerateJSONWithRetry_RetryThenSuccess(t *testing.T) {
	llm := &scriptedLLM{replies: []string{"garbage", `{"topics":[]}`}}
	var out topicsResponse
	if err := generateJSONWithRetry(context.Background(), llm, "u", "s", &out); err != nil {
		t.Fatalf("generateJSONWithRetry: %v", err)
	}
	if llm.calls != 2 {
		t.Errorf("calls = %d, want 2", llm.calls)
	}
}

func TestGenerateJSONWithRetry_BothInvalid(t *testing.T) {
	llm := &scriptedLLM{replies: []string{"garbage", "more garbage"}}
	var out topicsResponse
	if err := generateJSONWithRetry(context.Background(), llm, "u", "s", &out); err == nil {
		t.Fatal("expected error after failed retry")
	}
	if llm.calls != 2 {
		t.Errorf("calls = %d, want 2", llm.calls)
	}
}

func TestGenerateJSONWithRetry_UpstreamErrors(t *testing.T) {
	first := &scriptedLLM{errs: []error{errors.New("boom")}}
	var out topicsResponse
	if err := generateJSONWithRetry(context.Background(), first, "u", "s", &out); err == nil {
		t.Fatal("expected upstream error")
	}
	if first.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry after upstream error)", first.calls)
	}

	second := &scriptedLLM{replies: []string{"garbage"}, errs: []error{nil, errors.New("boom2")}}
	if err := generateJSONWithRetry(context.Background(), second, "u", "s", &out); err == nil {
		t.Fatal("expected upstream error on retry")
	}
	if second.calls != 2 {
		t.Errorf("calls = %d, want 2", second.calls)
	}
}

func testMessages() []model.Message {
	return []model.Message{
		{Role: "user", Content: "How does Docker networking work?"},
		{Role: "assistant", Content: "Bridge mode isolates containers."},
		{Role: "user", Content: "And host mode?"},
		{Role: "assistant", Content: "Host mode shares the host network stack."},
	}
}

func TestFormatConversation_MessagesNumbered(t *testing.T) {
	chat := model.Chat{Content: "flat", Messages: testMessages()}
	text, indexMap := formatConversation(chat, 0)
	if !strings.Contains(text, "[1] user: How does Docker networking work?") {
		t.Errorf("missing numbered first message: %q", text)
	}
	if !strings.Contains(text, "[4] assistant: Host mode shares the host network stack.") {
		t.Errorf("missing numbered last message: %q", text)
	}
	if len(indexMap) != 4 {
		t.Fatalf("len(indexMap) = %d, want 4", len(indexMap))
	}
	for i, v := range indexMap {
		if v != i+1 {
			t.Errorf("indexMap[%d] = %d, want identity %d", i, v, i+1)
		}
	}
}

func TestFormatConversation_FlatFallback(t *testing.T) {
	chat := model.Chat{Content: "flat content"}
	text, indexMap := formatConversation(chat, 0)
	if text != "flat content" {
		t.Errorf("text = %q, want flat content", text)
	}
	if indexMap != nil {
		t.Errorf("indexMap = %v, want nil for flat content", indexMap)
	}
}

func TestTruncateMessages(t *testing.T) {
	msgs := make([]model.Message, 6)
	for i := range msgs {
		msgs[i] = model.Message{Role: "assistant", Content: strings.Repeat("x", 30)}
	}

	// Disabled when maxRunes <= 0.
	if out, idx := truncateMessages(msgs, 0); len(out) != 6 || len(idx) != 6 {
		t.Errorf("maxRunes=0 must not truncate, got %d messages", len(out))
	}

	// Each message costs ~47 runes (30 content + 9 role + 8 overhead); total
	// ~282. Budget 150 -> head keeps 2 messages, tail keeps 1, marker in the
	// middle.
	out, idx := truncateMessages(msgs, 150)
	if len(out) != 4 || len(idx) != 4 {
		t.Fatalf("len(out) = %d, want 4 (head 2 + marker + tail 1)", len(out))
	}
	if !strings.Contains(out[2].Content, "truncated") {
		t.Errorf("marker message missing or wrong: %q", out[2].Content)
	}
	if idx[0] != 1 || idx[1] != 2 || idx[2] != 0 || idx[3] != 6 {
		t.Errorf("indexMap = %v, want [1 2 0 6]", idx)
	}

	// Budgets that fit everything return the input unchanged.
	out, idx = truncateMessages(msgs, 10000)
	if len(out) != 6 {
		t.Errorf("large budget must keep all messages, got %d", len(out))
	}
	for i, v := range idx {
		if v != i+1 {
			t.Errorf("indexMap[%d] = %d, want identity", i, v)
		}
	}
}

func TestExtractTopics_MessageRefsAndExcerpts(t *testing.T) {
	llm := &scriptedLLM{replies: []string{
		`{"topics":[{"title":"Docker networking","brief":"bridge vs host modes","messages":[1,4,99,4]}]}`,
	}}
	svc := newTestAnalyseService(llm, 0)
	chat := testChat("ignored flat content")
	chat.Messages = testMessages()

	topics, err := svc.extractTopics(context.Background(), chat)
	if err != nil {
		t.Fatalf("extractTopics: %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1", len(topics))
	}
	got := topics[0]
	if len(got.MessageRefs) != 2 || got.MessageRefs[0] != 1 || got.MessageRefs[1] != 4 {
		t.Errorf("MessageRefs = %v, want [1 4] (out-of-range and duplicates dropped)", got.MessageRefs)
	}
	wantExcerpts := []string{
		"user: How does Docker networking work?",
		"assistant: Host mode shares the host network stack.",
	}
	if len(got.SourceExcerpts) != 2 ||
		got.SourceExcerpts[0] != wantExcerpts[0] ||
		got.SourceExcerpts[1] != wantExcerpts[1] {
		t.Errorf("SourceExcerpts = %v, want %v", got.SourceExcerpts, wantExcerpts)
	}
	if !strings.Contains(llm.lastUser, "[1] user:") {
		t.Errorf("prompt does not render numbered messages: %q", llm.lastUser)
	}
	if !strings.Contains(llm.lastSystem, `"messages": [i, ...]`) {
		t.Errorf("system prompt missing messages addendum: %q", llm.lastSystem)
	}
}

func TestExtractTopics_NoMessagesLegacy(t *testing.T) {
	llm := &scriptedLLM{replies: []string{
		`{"topics":[{"title":"SQLite WAL","brief":"write-ahead logging trade-offs"}]}`,
	}}
	svc := newTestAnalyseService(llm, 0)

	topics, err := svc.extractTopics(context.Background(), testChat("hello"))
	if err != nil {
		t.Fatalf("extractTopics: %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1", len(topics))
	}
	if topics[0].MessageRefs != nil || topics[0].SourceExcerpts != nil {
		t.Errorf("legacy chat must carry no provenance, got refs=%v excerpts=%v",
			topics[0].MessageRefs, topics[0].SourceExcerpts)
	}
	if strings.Contains(llm.lastSystem, `"messages": [i, ...]`) {
		t.Error("system prompt must not include the messages addendum for flat chats")
	}
}
