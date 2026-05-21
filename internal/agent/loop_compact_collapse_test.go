package agent

import (
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

func TestCollapseToolCallsForSummary_PreservesToolCallsAndResults(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: "find recent docs"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: "call_1", Name: "web_search", Arguments: map[string]any{"query": "golang context.WithoutCancel"}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "5 hits found. top: https://example.com/foo"},
		{Role: "assistant", Content: "based on the search results, here are the highlights"},
		{Role: "user", Content: "great, look up the second one too"},
	}

	flat := collapseToolCallsForSummary(msgs)

	// User messages preserved
	if !strings.Contains(flat, "user: find recent docs") {
		t.Fatalf("expected user message preserved\n---\n%s\n---", flat)
	}
	// Tool call surfaces with name + args
	if !strings.Contains(flat, "assistant called tool: web_search(") {
		t.Fatalf("expected tool call name preserved\n---\n%s\n---", flat)
	}
	// Tool result preview surfaces
	if !strings.Contains(flat, "5 hits found") {
		t.Fatalf("expected tool result preview preserved\n---\n%s\n---", flat)
	}
	// Plain assistant text preserved
	if !strings.Contains(flat, "based on the search results") {
		t.Fatalf("expected plain assistant content preserved\n---\n%s\n---", flat)
	}
}

func TestCollapseToolCallsForSummary_TruncatesLargeResults(t *testing.T) {
	bigResult := strings.Repeat("X", 5_000)
	msgs := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c1", Name: "exec"}}},
		{Role: "tool", ToolCallID: "c1", Content: bigResult},
	}
	flat := collapseToolCallsForSummary(msgs)
	if strings.Count(flat, "X") > maxToolResultPreviewChars+5 {
		t.Fatalf("expected truncation to ~%d chars, full body leaked into summary", maxToolResultPreviewChars)
	}
	if !strings.Contains(flat, "...") {
		t.Fatalf("expected truncation ellipsis in preview")
	}
}

func TestCollapseToolCallsForSummary_OrphanCallShowsNoResult(t *testing.T) {
	msgs := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "orphan", Name: "do_thing"}}},
	}
	flat := collapseToolCallsForSummary(msgs)
	if !strings.Contains(flat, "[no result]") {
		t.Fatalf("expected orphan tool call to surface '[no result]'\n---\n%s\n---", flat)
	}
}

func TestSafeSplitIndex_DoesNotSplitToolCallPair(t *testing.T) {
	// History: 3 normal turns + assistant.tool_call + tool result + user (cut here)
	// Naive split at len-1 would leave tool result at head of kept → orphan.
	msgs := []providers.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c1", Name: "t"}}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "user", Content: "u3"},
	}

	// Naive cut at index 5 (keep 1 message) — index 5 is the user, but index
	// 4 (last summarized) is tool. safeSplitIndex must keep walking back to
	// land before the assistant.tool_calls block.
	got := safeSplitIndex(msgs, 5)
	if got >= 4 {
		t.Fatalf("expected safe split to land before tool_call pair (index ≤3), got %d", got)
	}
	// Verify the head of the kept slice is NOT a tool message.
	if msgs[got].Role == "tool" {
		t.Fatalf("safe split must not leave a tool message at head of kept slice")
	}
	// Verify the tail of the summarized slice is NOT an assistant with tool_calls.
	if got > 0 {
		tail := msgs[got-1]
		if tail.Role == "assistant" && len(tail.ToolCalls) > 0 {
			t.Fatalf("safe split must not put an assistant.tool_calls at tail of summarized slice")
		}
	}
}

func TestSafeSplitIndex_StableWhenAlreadyClean(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
	}
	got := safeSplitIndex(msgs, 2)
	if got != 2 {
		t.Fatalf("clean boundary should be preserved, got %d", got)
	}
}

// --- KeepLast default bump ---

func TestDefaultKeepLastMessages_IsBumpedTo16(t *testing.T) {
	if config.DefaultKeepLastMessages != 16 {
		t.Fatalf("expected DefaultKeepLastMessages=16 (industry default), got %d", config.DefaultKeepLastMessages)
	}
}

func TestDefaultSummarizerModelAlias_IsEmpty(t *testing.T) {
	// Empty default means: reuse the agent's primary model. Operators can
	// override via CompactionConfig.SummarizerModel = "fast" if cost matters.
	if config.DefaultSummarizerModelAlias != "" {
		t.Fatalf("expected DefaultSummarizerModelAlias=\"\" (reuse primary model), got %q", config.DefaultSummarizerModelAlias)
	}
}
