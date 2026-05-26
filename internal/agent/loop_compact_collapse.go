package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// maxToolResultPreviewChars is the max length of each tool-result preview that
// gets fed into the summarizer. Long tool outputs (file dumps, large API
// responses) are truncated — the goal is to preserve "what ran and what came
// back", not the raw payload.
const maxToolResultPreviewChars = 500

// collapseToolCallsForSummary builds a flat text representation of the
// conversation that includes tool invocations and (truncated) tool results.
//
// Without this, the legacy summarizer prompt-builder silently DROPPED every
// `role=tool` message and every `role=assistant` message that only carried
// tool_calls (Content is usually empty for the latter). The agent would
// forget which tools ran and what they returned after a compaction cycle.
//
// Output shape per pair (one per line):
//
//	user: <text>
//	assistant: <text>
//	assistant called tool: <name>(<args>) → result_preview: "<first 500 chars>"
//
// If a tool call has no matching tool result in the slice the line still emits
// the call with `result_preview: "[no result]"` so the summarizer sees the
// orphan and can flag it.
func collapseToolCallsForSummary(messages []providers.Message) string {
	var sb strings.Builder

	// Build a tool-call-id → result-preview lookup so each call line can be
	// emitted with its result on the same line. Walking forward and looking up
	// the next tool message would also work but using a map keeps the code
	// resilient to ordering anomalies created by sanitizeHistory's repair pass.
	resultByID := make(map[string]string, len(messages))
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultByID[m.ToolCallID] = truncatePreview(m.Content)
		}
	}

	for _, m := range messages {
		switch m.Role {
		case "user":
			fmt.Fprintf(&sb, "user: %s\n", strings.TrimSpace(m.Content))
		case "assistant":
			sanitized := strings.TrimSpace(SanitizeAssistantContent(m.Content))
			if sanitized != "" {
				fmt.Fprintf(&sb, "assistant: %s\n", sanitized)
			}
			for _, tc := range m.ToolCalls {
				args := formatToolArgs(tc.Arguments)
				preview, ok := resultByID[tc.ID]
				if !ok {
					preview = "[no result]"
				}
				fmt.Fprintf(&sb, "assistant called tool: %s(%s) → result_preview: %q\n",
					tc.Name, args, preview)
			}
		case "tool":
			// Already attached to its parent assistant call line above —
			// don't emit a duplicate. But if (somehow) a tool message had no
			// matching tool_call in this slice, surface it so the orphan is
			// not silently dropped.
			if m.ToolCallID == "" {
				continue
			}
			// If we already emitted it under its call, skip.
			// Detect "already emitted" by checking whether the slice contains
			// a preceding assistant.ToolCalls with this ID — cheap pass.
			emitted := false
			for _, prev := range messages {
				if prev.Role != "assistant" {
					continue
				}
				for _, tc := range prev.ToolCalls {
					if tc.ID == m.ToolCallID {
						emitted = true
						break
					}
				}
				if emitted {
					break
				}
			}
			if !emitted {
				fmt.Fprintf(&sb, "orphan tool result (id=%s): %q\n",
					m.ToolCallID, truncatePreview(m.Content))
			}
		}
	}

	return sb.String()
}

func truncatePreview(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxToolResultPreviewChars {
		return s
	}
	return s[:maxToolResultPreviewChars] + "..."
}

// formatToolArgs renders a tool's argument map as `k1="v1", k2="v2"`. Stable
// shape regardless of map iteration order is not required for summarization —
// the LLM consumes it as free text.
func formatToolArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		var rendered string
		switch t := v.(type) {
		case string:
			rendered = fmt.Sprintf("%q", truncatePreview(t))
		default:
			b, err := json.Marshal(v)
			if err != nil {
				rendered = fmt.Sprintf("%v", v)
			} else {
				rendered = truncatePreview(string(b))
			}
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, rendered))
	}
	return strings.Join(parts, ", ")
}

// safeSplitIndex walks backward from `splitIdx` (where the caller would naively
// cut the history into [summarize | keep]) until landing on a boundary that
// does NOT split a tool_call → tool_result pair. Used by both the in-place
// compaction path and the post-turn summarize path so they share identical
// boundary semantics — preventing the orphaned tool messages that previously
// required a defensive sanitizeHistory repair on the next LLM call.
//
// Returns the adjusted index. Caller MUST treat a return value <=1 as
// "cannot safely compact" (no meaningful history left).
func safeSplitIndex(messages []providers.Message, splitIdx int) int {
	if splitIdx > len(messages) {
		splitIdx = len(messages)
	}
	for splitIdx > 0 {
		m := messages[splitIdx-1]
		// If the message immediately before the cut is an assistant that
		// issued tool_calls, we cannot cut here — the tool results that
		// follow would become orphaned at the head of the kept slice. Move
		// the cut left so the tool-call + tool-result pair stays together.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			splitIdx--
			continue
		}
		// If the message immediately before the cut is a tool result, it
		// belongs to a preceding assistant.tool_calls block. Walk left so
		// the next iteration catches the assistant and moves past it too.
		if m.Role == "tool" {
			splitIdx--
			continue
		}
		// If the message AT splitIdx (first kept message) is a tool result,
		// the kept slice would start with an orphan. Move left.
		if splitIdx < len(messages) && messages[splitIdx].Role == "tool" {
			splitIdx--
			continue
		}
		break
	}
	return splitIdx
}
