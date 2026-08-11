package handler

import (
	"encoding/json"
	"net/http"

	"github.com/mindcache-fyi/server/internal/cache"
	"github.com/mindcache-fyi/server/internal/service"
)

// DebugHandler exposes internal state for debugging.
type DebugHandler struct {
	llm service.LLM
	kv  *cache.KVCache
}

// NewDebugHandler creates a DebugHandler.
func NewDebugHandler(llm service.LLM, kv *cache.KVCache) *DebugHandler {
	return &DebugHandler{llm: llm, kv: kv}
}

// LLMConfig returns the current LLM configuration.
// @Summary Return LLM provider configuration (no API key)
// @Tags debug
// @Produce json
// @Success 200 {object} service.LLMConfig
// @Router /v1/api/debug/llm [get]
func (h *DebugHandler) LLMConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.llm.GetConfig())
}

// KVCache returns all cached chats and topics.
// @Summary View all cached chats and topics (KV debug)
// @Tags debug
// @Produce json
// @Success 200 {object} map[string]any
// @Router /v1/api/debug/kvcache [get]
func (h *DebugHandler) KVCache(w http.ResponseWriter, r *http.Request) {
	chats := parseKVEntries(h.kv.RangeByPrefix("c_"))
	topics := parseKVEntries(h.kv.RangeByPrefix("t_"))
	writeJSON(w, http.StatusOK, map[string]any{
		"chats":  chats,
		"topics": topics,
	})
}

func parseKVEntries(entries map[string]string) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(entries))
	for k, v := range entries {
		if json.Valid([]byte(v)) {
			result[k] = json.RawMessage(v)
		} else {
			raw, _ := json.Marshal(v)
			result[k] = raw
		}
	}
	return result
}
