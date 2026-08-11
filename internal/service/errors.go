package service

import "errors"

var (
	// ErrLLMUpstream indicates the LLM endpoint itself failed (connection
	// error, non-2xx response, timeout while calling the model, ...).
	ErrLLMUpstream = errors.New("llm upstream error")

	// ErrLLMResponse indicates the LLM answered but the response could not
	// be used (unparseable or malformed JSON).
	ErrLLMResponse = errors.New("llm response invalid")
)
