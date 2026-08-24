package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewStartShutdown(t *testing.T) {
	cfg := &Config{
		Env:               "production",
		Port:              "0",
		DBPath:            filepath.Join(t.TempDir(), "test.db"),
		StorageURL:        "mem://",
		LLMBaseURL:        "http://127.0.0.1:9/v1",
		LLMModel:          "test-model",
		LLMMaxConcurrency: 1,
	}

	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	addr := app.Addr()
	if addr == "" {
		t.Fatal("Addr returned empty string after Start")
	}

	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := &Config{
		Env:        "production",
		Port:       "0",
		DBPath:     filepath.Join(t.TempDir(), "test.db"),
		StorageURL: "file://.",
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error for missing LLM config in production")
	}
}

// mockLLMServer speaks OpenAI-compatible chat completions, returning
// scripted assistant replies in order. It answers both streaming (SSE) and
// non-streaming requests, and records every request body.
type mockLLMServer struct {
	mu      sync.Mutex
	replies []string
	bodies  []string
	calls   int
}

func (m *mockLLMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	streaming := bytes.Contains(body, []byte(`"stream":true`))

	m.mu.Lock()
	i := m.calls
	m.calls++
	m.bodies = append(m.bodies, string(body))
	reply := ""
	if i < len(m.replies) {
		reply = m.replies[i]
	}
	m.mu.Unlock()

	content, _ := json.Marshal(reply)
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":null}]}\n\n", content)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
}

func startTestApp(t *testing.T, llmURL string, mutate ...func(*Config)) string {
	t.Helper()
	cfg := &Config{
		Env:               "production",
		Port:              "0",
		DBPath:            filepath.Join(t.TempDir(), "test.db"),
		StorageURL:        "mem://",
		LLMBaseURL:        llmURL,
		LLMModel:          "test-model",
		LLMMaxConcurrency: 1,
	}
	for _, m := range mutate {
		m(cfg)
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
	})
	return "http://" + app.Addr()
}

func postJSON(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, body: %s", url, resp.StatusCode, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s response: %v", url, err)
	}
	return out
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, body: %s", url, resp.StatusCode, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s response: %v", url, err)
	}
	return out
}

// TestAnalyseAndCreateWithProvenance exercises the v2 payload end to end:
// structured messages -> analyse (topic refs + excerpts) -> create ->
// get (sources.json provenance).
func TestAnalyseAndCreateWithProvenance(t *testing.T) {
	topicsReply := `{"topics":[{"title":"Docker networking","brief":"bridge vs host modes","messages":[1,2]}]}`
	extractReply := "## Docker networking\n\nBridge mode isolates containers."
	mock := &mockLLMServer{replies: []string{topicsReply, extractReply}}
	llmSrv := httptest.NewServer(mock)
	defer llmSrv.Close()

	base := startTestApp(t, llmSrv.URL+"/v1")

	analyseBody := `{
		"chat": {
			"chatId": "chat-x",
			"provider": "chatgpt",
			"sourceUrl": "https://chatgpt.com/c/1",
			"title": "Docker chat",
			"content": "user: How does Docker networking work?\n\n-----\n\nassistant: Bridge mode isolates containers.",
			"messages": [
				{"role": "user", "content": "How does Docker networking work?"},
				{"role": "assistant", "content": "Bridge mode isolates containers."}
			]
		}
	}`
	analyse := postJSON(t, base+"/v1/api/analyse", analyseBody)

	topics, ok := analyse["topics"].([]any)
	if !ok || len(topics) != 1 {
		t.Fatalf("topics = %#v, want 1 topic", analyse["topics"])
	}
	topic := topics[0].(map[string]any)
	topicID := topic["topicId"].(string)
	refs, ok := topic["messageRefs"].([]any)
	if !ok || len(refs) != 2 {
		t.Fatalf("messageRefs = %#v, want [1 2]", topic["messageRefs"])
	}
	if refs[0].(float64) != 1 || refs[1].(float64) != 2 {
		t.Errorf("messageRefs = %v, want [1 2]", refs)
	}
	excerpts, ok := topic["sourceExcerpts"].([]any)
	if !ok || len(excerpts) != 2 {
		t.Fatalf("sourceExcerpts = %#v, want 2 entries", topic["sourceExcerpts"])
	}
	if excerpts[0].(string) != "user: How does Docker networking work?" {
		t.Errorf("excerpts[0] = %q", excerpts[0].(string))
	}

	create := postJSON(t, base+"/v1/api/create", `{"topicId":"`+topicID+`"}`)
	if create["success"] != true {
		t.Fatalf("create failed: %#v", create)
	}
	mcID := create["mindcacheId"].(string)

	got := getJSON(t, base+"/v1/api/get/"+mcID)
	sources, ok := got["sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("sources = %#v, want exactly one record", got["sources"])
	}
	rec := sources[0].(map[string]any)
	if rec["chatId"] != "chat-x" || rec["sourceUrl"] != "https://chatgpt.com/c/1" {
		t.Errorf("source record identity wrong: %#v", rec)
	}
	if rec["topicTitle"] != "Docker networking" {
		t.Errorf("topicTitle = %#v", rec["topicTitle"])
	}
	recExcerpts, ok := rec["excerpts"].([]any)
	if !ok || len(recExcerpts) != 2 {
		t.Errorf("record excerpts = %#v, want 2 entries", rec["excerpts"])
	}
}

// TestAnalyseLegacyFlatContent keeps the old payload working: no messages,
// no provenance fields.
func TestAnalyseLegacyFlatContent(t *testing.T) {
	topicsReply := `{"topics":[{"title":"SQLite WAL","brief":"write-ahead logging trade-offs"}]}`
	mock := &mockLLMServer{replies: []string{topicsReply}}
	llmSrv := httptest.NewServer(mock)
	defer llmSrv.Close()

	base := startTestApp(t, llmSrv.URL+"/v1")

	analyseBody := `{
		"chat": {
			"chatId": "chat-legacy",
			"provider": "claude",
			"sourceUrl": "https://claude.ai/chat/1",
			"title": "SQLite",
			"content": "user: What is WAL mode?"
		}
	}`
	analyse := postJSON(t, base+"/v1/api/analyse", analyseBody)

	topics := analyse["topics"].([]any)
	if len(topics) != 1 {
		t.Fatalf("topics = %#v, want 1 topic", analyse["topics"])
	}
	topic := topics[0].(map[string]any)
	if _, present := topic["messageRefs"]; present {
		t.Errorf("legacy analyse must omit messageRefs, got %#v", topic["messageRefs"])
	}
	if _, present := topic["sourceExcerpts"]; present {
		t.Errorf("legacy analyse must omit sourceExcerpts, got %#v", topic["sourceExcerpts"])
	}
}

// routedEmbeddingsServer answers /embeddings with vectors chosen by input
// content: anything mentioning "alpha" maps to [1,0], "beta" to [0,1], and
// everything else (including the startup probe) to [1,0].
type routedEmbeddingsServer struct{}

func (s *routedEmbeddingsServer) vectorFor(text string) []float64 {
	if strings.Contains(text, "beta") {
		return []float64{0, 1}
	}
	return []float64{1, 0}
}

func (s *routedEmbeddingsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/embeddings" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var inputs []string
	if err := json.Unmarshal(req.Input, &inputs); err != nil {
		var single string
		if err := json.Unmarshal(req.Input, &single); err == nil {
			inputs = []string{single}
		}
	}

	data := make([]map[string]any, 0, len(inputs))
	for i, in := range inputs {
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     i,
			"embedding": s.vectorFor(in),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
		"model":  "test-embed",
	})
}

// TestEmbeddingMatchingEndToEnd verifies retrieval-augmented matching: after
// creating two mindcaches with orthogonal embeddings, analysing a chat whose
// topic resembles the first one must send only that mindcache to the match
// call.
func TestEmbeddingMatchingEndToEnd(t *testing.T) {
	llm := &mockLLMServer{replies: []string{
		// analyse A (topics only — collection empty, no match call)
		`{"topics":[{"title":"Alpha Net","brief":"alpha networking"}]}`,
		// create A (markdown extraction)
		"# Alpha networking\n\nBridge details.",
		// analyse B (topics only — but now there IS one mindcache...)
		`{"topics":[{"title":"Beta Cook","brief":"beta cooking"}]}`,
		// ...so analyse B also triggers a match call: nothing matches
		`{"matches":{}}`,
		// create B (markdown extraction)
		"# Beta cooking\n\nSourdough notes.",
		// analyse C topics
		`{"topics":[{"title":"Alpha Net Deep","brief":"alpha networking details"}]}`,
		// analyse C match — only candidate A is in the prompt; it matches
		`{"matches":{"1":[1]}}`,
	}}
	llmSrv := httptest.NewServer(llm)
	defer llmSrv.Close()
	embedSrv := httptest.NewServer(&routedEmbeddingsServer{})
	defer embedSrv.Close()

	base := startTestApp(t, llmSrv.URL+"/v1", func(cfg *Config) {
		cfg.EmbedBaseURL = embedSrv.URL + "/v1"
		cfg.EmbedAPIKey = "key"
		cfg.EmbedModel = "test-embed"
		cfg.MatchCandidateK = 5
		cfg.MinEmbedCollection = 1
	})

	createMindcache := func(chatID, chatContent string) string {
		t.Helper()
		analyse := postJSON(t, base+"/v1/api/analyse", `{
			"chat": {"chatId":"`+chatID+`","provider":"chatgpt",
				"sourceUrl":"https://chatgpt.com/c/`+chatID+`",
				"title":"`+chatID+`","content":`+strconv.Quote(chatContent)+`}
		}`)
		topicID := analyse["topics"].([]any)[0].(map[string]any)["topicId"].(string)
		create := postJSON(t, base+"/v1/api/create", `{"topicId":"`+topicID+`"}`)
		if create["success"] != true {
			t.Fatalf("create failed: %#v", create)
		}
		return create["mindcacheId"].(string)
	}

	mcA := createMindcache("chat-a", "user: tell me about alpha networking\n\n-----\n\nassistant: alpha uses bridges")
	mcB := createMindcache("chat-b", "user: tell me about beta cooking\n\n-----\n\nassistant: beta needs flour")
	if mcA == mcB {
		t.Fatalf("expected distinct mindcache ids, got %s", mcA)
	}

	analyseC := postJSON(t, base+"/v1/api/analyse", `{
		"chat": {"chatId":"chat-c","provider":"chatgpt",
			"sourceUrl":"https://chatgpt.com/c/chat-c",
			"title":"chat-c","content":"user: more alpha networking details\n\n-----\n\nassistant: alpha bridge internals"}
	}`)
	topicC := analyseC["topics"].([]any)[0].(map[string]any)["topicId"].(string)

	matches, ok := analyseC["topicMindcacheMap"].(map[string]any)
	if !ok {
		t.Fatalf("topicMindcacheMap = %#v", analyseC["topicMindcacheMap"])
	}
	got, ok := matches[topicC].([]any)
	if !ok || len(got) != 1 || got[0].(string) != mcA {
		t.Fatalf("matches[%s] = %#v, want [%s]", topicC, matches[topicC], mcA)
	}

	// The match request (final recorded body) must contain only candidate A.
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.bodies) == 0 {
		t.Fatal("no LLM requests recorded")
	}
	matchBody := llm.bodies[len(llm.bodies)-1]
	if !strings.Contains(matchBody, "alpha networking") {
		t.Errorf("match prompt missing candidate A brief: %q", matchBody)
	}
	if strings.Contains(matchBody, "beta cooking") {
		t.Errorf("match prompt must not contain non-candidate B: %q", matchBody)
	}
}

// TestAnalyseUnifiedEndToEnd verifies the single-call pipeline: one LLM call
// per analyse produces topics with matches, and create/get still work.
func TestAnalyseUnifiedEndToEnd(t *testing.T) {
	llm := &mockLLMServer{replies: []string{
		// analyse A — single unified call, empty collection
		`{"topics":[{"title":"Alpha Net","brief":"alpha networking"}]}`,
		// create A — markdown extraction
		"# Alpha networking\n\nBridge details.",
		// analyse B — single unified call, collection contains A
		`{"topics":[{"title":"Alpha Again","brief":"alpha networking details","matches":[1]}]}`,
	}}
	llmSrv := httptest.NewServer(llm)
	defer llmSrv.Close()

	base := startTestApp(t, llmSrv.URL+"/v1", func(cfg *Config) {
		cfg.AnalyseMode = AnalyseModeUnified
	})

	analyseA := postJSON(t, base+"/v1/api/analyse", `{
		"chat": {"chatId":"chat-a","provider":"chatgpt",
			"sourceUrl":"https://chatgpt.com/c/chat-a",
			"title":"chat-a","content":"user: alpha networking\n\n-----\n\nassistant: bridges"}
	}`)
	topicA := analyseA["topics"].([]any)[0].(map[string]any)["topicId"].(string)
	createA := postJSON(t, base+"/v1/api/create", `{"topicId":"`+topicA+`"}`)
	if createA["success"] != true {
		t.Fatalf("create failed: %#v", createA)
	}
	mcA := createA["mindcacheId"].(string)

	analyseB := postJSON(t, base+"/v1/api/analyse", `{
		"chat": {"chatId":"chat-b","provider":"chatgpt",
			"sourceUrl":"https://chatgpt.com/c/chat-b",
			"title":"chat-b","content":"user: more alpha details\n\n-----\n\nassistant: bridge internals"}
	}`)
	topicB := analyseB["topics"].([]any)[0].(map[string]any)["topicId"].(string)

	// One unified call per analyse + one extraction for create = 3 calls.
	llm.mu.Lock()
	if llm.calls != 3 {
		t.Errorf("llm calls = %d, want 3 (unified analyse is a single call)", llm.calls)
	}
	unifiedBody := llm.bodies[len(llm.bodies)-1]
	llm.mu.Unlock()
	if !strings.Contains(unifiedBody, "MINDCACHES") || !strings.Contains(unifiedBody, "alpha networking") {
		t.Errorf("unified prompt must list the existing mindcache: %q", unifiedBody)
	}

	matches, ok := analyseB["topicMindcacheMap"].(map[string]any)
	if !ok {
		t.Fatalf("topicMindcacheMap = %#v", analyseB["topicMindcacheMap"])
	}
	got, ok := matches[topicB].([]any)
	if !ok || len(got) != 1 || got[0].(string) != mcA {
		t.Errorf("matches[%s] = %#v, want [%s]", topicB, matches[topicB], mcA)
	}
	mindcaches, ok := analyseB["mindcaches"].([]any)
	if !ok || len(mindcaches) != 1 {
		t.Errorf("mindcaches = %#v, want the single matched entry", analyseB["mindcaches"])
	}

	// Provenance path is mode-independent.
	get := getJSON(t, base+"/v1/api/get/"+mcA)
	if get["mainContent"] == "" {
		t.Error("mainContent must be stored")
	}
}
