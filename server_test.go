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
// non-streaming requests.
type mockLLMServer struct {
	mu      sync.Mutex
	replies []string
	calls   int
}

func (m *mockLLMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	streaming := bytes.Contains(body, []byte(`"stream":true`))

	m.mu.Lock()
	i := m.calls
	m.calls++
	reply := ""
	if i < len(m.replies) {
		reply = m.replies[i]
	}
	m.mu.Unlock()

	content, _ := json.Marshal(reply)
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":null}]}\n\n", content)
		fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
}

func startTestApp(t *testing.T, llmURL string) string {
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
