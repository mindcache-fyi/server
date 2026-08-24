package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSearchEndToEnd captures a chat through analyse -> create, then asserts
// the full-text search endpoint finds the new mindcache by content, brief,
// and short CJK queries, and that deletion removes it from results.
func TestSearchEndToEnd(t *testing.T) {
	topicsReply := `{"topics":[{"title":"Docker networking","brief":"bridge vs host modes"}]}`
	extractReply := "## Docker networking\n\nBridge mode isolates containers from the host."
	mock := &mockLLMServer{replies: []string{topicsReply, extractReply}}
	llmSrv := httptest.NewServer(mock)
	defer llmSrv.Close()

	base := startTestApp(t, llmSrv.URL+"/v1")

	analyseBody := `{
		"chat": {
			"chatId": "chat-search",
			"provider": "chatgpt",
			"sourceUrl": "https://chatgpt.com/c/9",
			"title": "Docker chat",
			"content": "user: Docker networking?\n\nassistant: Bridge mode isolates containers."
		}
	}`
	analyse := postJSON(t, base+"/v1/api/analyse", analyseBody)
	topicID := analyse["topics"].([]any)[0].(map[string]any)["topicId"].(string)

	create := postJSON(t, base+"/v1/api/create", `{"topicId":"`+topicID+`"}`)
	if create["success"] != true {
		t.Fatalf("create failed: %#v", create)
	}
	mcID := create["mindcacheId"].(string)

	search := func(q string) map[string]any {
		t.Helper()
		resp, err := http.Get(base + "/v1/api/search?q=" + q)
		if err != nil {
			t.Fatalf("GET search: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("search %s status = %d", q, resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode search response: %v", err)
		}
		return out
	}

	resultsOf := func(m map[string]any) []any {
		t.Helper()
		rs, ok := m["results"].([]any)
		if !ok {
			t.Fatalf("results missing: %#v", m)
		}
		return rs
	}

	// Content match via trigram index.
	rs := resultsOf(search("isolates%20containers"))
	if len(rs) != 1 || rs[0].(map[string]any)["mindcache"].(map[string]any)["id"] != mcID {
		t.Fatalf("content search results = %#v, want exactly %s", rs, mcID)
	}
	hit := rs[0].(map[string]any)
	snippet, _ := hit["snippet"].(string)
	if !strings.Contains(snippet, "\x01") {
		t.Errorf("snippet = %q, want highlight markers", snippet)
	}

	// Brief match.
	rs = resultsOf(search("host%20modes"))
	if len(rs) != 1 {
		t.Fatalf("brief search results = %#v, want 1", rs)
	}

	// Two-character CJK query routes through the LIKE fallback; nothing
	// matches this content but the query must not error.
	search("%E8%BF%9E%E6%8E%A5%E6%B1%A0") // 连接池

	// Missing query parameter is a client error.
	resp, err := http.Get(base + "/v1/api/search")
	if err != nil {
		t.Fatalf("GET search without q: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status without q = %d, want 400", resp.StatusCode)
	}

	// Deletion removes the entry from the index.
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/api/delete/"+mcID, nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = delResp.Body.Close()
	rs = resultsOf(search("isolates%20containers"))
	if len(rs) != 0 {
		t.Fatalf("results after delete = %#v, want empty", rs)
	}
}
