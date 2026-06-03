package providers

import "testing"

// TestBuildRequestBody_AliasResolvesToGeminiEchoesSignature exercises the
// alias-aware capability detection in buildRequestBody. When a user-created
// agent is pinned to a non-Gemini-looking alias like "default" — but the
// LLM gateway resolves that alias to a Vertex/Gemini upstream — goclaw must
// still echo thought_signature back. Without this, Vertex rejects the next
// tool_call with INVALID_ARGUMENT ("missing thought_signature in functionCall
// parts") on user-created agents while system-seeded agents with literal
// model names like "gemini-3.5-flash" keep working.
//
// The fix consults the shared model registry — populated by the
// model_alias_fetcher from GET /v1/models — for the alias's true
// UpstreamProvider / UpstreamModel before treating the request as
// non-Gemini.
func TestBuildRequestBody_AliasResolvesToGeminiEchoesSignature(t *testing.T) {
	reg := NewInMemoryRegistry()
	reg.RegisterAlias("default", ModelSpec{
		ContextWindow:    200_000,
		UpstreamProvider: "vertex",
		UpstreamModel:    "google/gemini-3.5-flash",
	})

	// Provider name "llm-service" and model "default" both lack "gemini"
	// substring — the only signal that this is a Gemini upstream comes
	// from the registry.
	p := NewOpenAIProvider("llm-service", "key",
		"https://stg-web-agent-api.injecting.ai", "default").WithRegistry(reg)

	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "find weather"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{
					ID: "call_1", Name: "web_search",
					Arguments: map[string]any{"q": "weather"},
					Metadata:  map[string]string{"thought_signature": "AY89-sig"},
				},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
	}

	body := p.buildRequestBody("default", req, false)
	msgs, ok := body["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("messages: %T", body["messages"])
	}
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 msgs, got %d", len(msgs))
	}

	// The assistant tool_call must carry function.thought_signature on the
	// wire — that's the slot llm-service lifts to Vertex's extra_content.
	assistant := msgs[1]
	tcs, ok := assistant["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("assistant tool_calls missing or wrong type: %#v", assistant)
	}
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	fn, ok := tcs[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("function missing: %#v", tcs[0])
	}
	if fn["thought_signature"] != "AY89-sig" {
		t.Fatalf("thought_signature dropped on alias route; got %v want AY89-sig", fn["thought_signature"])
	}

	// And the role=tool message must carry `name` (FunctionResponse.name)
	// — Vertex's OpenAI-compat shim 400s without it. The gate that adds
	// `name` shares the same Gemini-detection path, so this confirms the
	// fix isn't only signature-shaped.
	toolMsg := msgs[2]
	if toolMsg["name"] != "web_search" {
		t.Fatalf("tool msg name missing on alias route; got %v want web_search", toolMsg["name"])
	}
}

// TestBuildRequestBody_AliasResolvesToNonGeminiDoesNotEchoSignature is the
// inverse: an alias that resolves to a non-Gemini upstream (e.g. OpenRouter
// deepseek) must NOT receive thought_signature, since downstream providers
// reject the unknown field with 422 Unprocessable Entity. The registry's
// UpstreamProvider tells us which side we're on.
func TestBuildRequestBody_AliasResolvesToNonGeminiDoesNotEchoSignature(t *testing.T) {
	reg := NewInMemoryRegistry()
	reg.RegisterAlias("fast", ModelSpec{
		ContextWindow:    200_000,
		UpstreamProvider: "openrouter",
		UpstreamModel:    "deepseek/deepseek-v4-flash",
	})

	p := NewOpenAIProvider("llm-service", "key",
		"https://stg-web-agent-api.injecting.ai", "fast").WithRegistry(reg)

	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "x"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{
					ID: "call_1", Name: "noop",
					// Stale metadata from a prior Gemini turn — shouldn't leak.
					Metadata: map[string]string{"thought_signature": "AY89-stale"},
				},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "ok"},
		},
	}

	body := p.buildRequestBody("fast", req, false)
	msgs := body["messages"].([]map[string]any)
	tcs := msgs[1]["tool_calls"].([]map[string]any)
	fn := tcs[0]["function"].(map[string]any)
	if _, present := fn["thought_signature"]; present {
		t.Fatalf("thought_signature must not be sent to non-Gemini upstream; got %v", fn["thought_signature"])
	}
}
