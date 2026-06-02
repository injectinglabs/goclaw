package providers

import (
	"testing"
)

// TestCollapseAllSiblingsUnsigned — fully unsigned tool_call cycle is collapsed.
// Defensive fallback for legacy Gemini variants that rejected unsigned echo-back.
func TestCollapseAllSiblingsUnsigned(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "do three tasks"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "c1", Name: "spawn"},
				{ID: "c2", Name: "spawn"},
				{ID: "c3", Name: "spawn"},
			},
		},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "tool", ToolCallID: "c3", Content: "r3"},
	}

	out := collapseToolCallsWithoutSig(msgs)

	// assistant.tool_calls stripped; tool results folded into a single user message.
	if len(out) != 2 {
		t.Fatalf("expected 2 messages after collapse, got %d: %#v", len(out), out)
	}
	if out[1].Role != "user" || out[1].Content == "" {
		t.Fatalf("expected folded user message, got %#v", out[1])
	}
}

// TestNoCollapseFirstSiblingSigned — assistant message has signature only on the
// first tool_call (Gemini's "one sig per thought-group" semantics for parallel
// function calls). The cycle MUST survive intact: the previous "any-empty
// triggers collapse" rule stripped these and caused an infinite spawn loop
// because Gemini, seeing tool reports arrive as plain user messages, re-derived
// the same plan and re-spawned the same N subagents each iteration.
func TestNoCollapseFirstSiblingSigned(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "do three tasks"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "c1", Name: "spawn", Metadata: map[string]string{"thought_signature": "AY89..."}},
				{ID: "c2", Name: "spawn"},
				{ID: "c3", Name: "spawn"},
			},
		},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "tool", ToolCallID: "c3", Content: "r3"},
	}

	out := collapseToolCallsWithoutSig(msgs)

	// Nothing collapsed — original structure preserved end-to-end.
	if len(out) != len(msgs) {
		t.Fatalf("expected %d messages preserved, got %d", len(msgs), len(out))
	}
	if len(out[1].ToolCalls) != 3 {
		t.Fatalf("expected 3 tool_calls preserved on assistant, got %d", len(out[1].ToolCalls))
	}
}

// TestNoCollapseLastSiblingSigned — same as above but signature on the last
// sibling. Position must not matter; presence of any one signature in the group
// is sufficient evidence the cycle is signed.
func TestNoCollapseLastSiblingSigned(t *testing.T) {
	msgs := []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "c1", Name: "spawn"},
				{ID: "c2", Name: "spawn"},
				{ID: "c3", Name: "spawn", Metadata: map[string]string{"thoughtSignature": "AY89..."}},
			},
		},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "tool", ToolCallID: "c3", Content: "r3"},
	}

	out := collapseToolCallsWithoutSig(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("expected preservation, got %d msgs", len(out))
	}
}

// TestNoCollapseWhenNoToolCalls — nothing to collapse on a plain text exchange.
func TestNoCollapseWhenNoToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	out := collapseToolCallsWithoutSig(msgs)
	if len(out) != 2 {
		t.Fatalf("expected pass-through, got %d", len(out))
	}
}
