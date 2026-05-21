package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// compactionSummaryPrompt is the structured summarization instruction used by both
// mid-loop compaction and background summarization. Matching OpenClaw TS compaction.ts
// MERGE_SUMMARIES_INSTRUCTIONS + IDENTIFIER_PRESERVATION_INSTRUCTIONS.
const compactionSummaryPrompt = `Summarize this conversation concisely for the AI agent to resume work.

MUST PRESERVE:
- Active tasks and their current status (in-progress, blocked, pending)
- Pending subagent tasks (IDs, labels, statuses) — agent needs to know what is still running
- Pending team task results awaiting delivery (task IDs, assignees, statuses)
- Any "waiting for..." state — do NOT drop expectations of future results
- Batch operation progress (e.g., "5/17 items completed")
- The last thing the user requested and what was being done about it
- Decisions made and their rationale
- TODOs, open questions, and constraints
- Any commitments or follow-ups promised

IDENTIFIER PRESERVATION:
Preserve all opaque identifiers exactly as written (no shortening or reconstruction),
including UUIDs, hashes, IDs, tokens, API keys, hostnames, IPs, ports, URLs, and file names.

PRIORITIZE recent context over older history. The agent needs to know
what it was doing, not just what was discussed.

Conversation to summarize:

`

// compactMessagesInPlace summarizes the first ~70% of messages into a condensed
// summary, keeping the last ~30% intact. Operates purely on the local messages
// slice — no session state touched, no locks needed.
// Returns nil on failure (caller keeps original messages).
func (l *Loop) compactMessagesInPlace(ctx context.Context, messages []providers.Message) []providers.Message {
	if len(messages) < 6 {
		return nil
	}

	// Resolve keepCount from compaction config (same defaults as maybeSummarize).
	keepCount := config.DefaultKeepLastMessages
	if l.compactionCfg != nil && l.compactionCfg.KeepLastMessages > 0 {
		keepCount = l.compactionCfg.KeepLastMessages
	}
	// Ensure we keep at least 30% of messages.
	if minKeep := len(messages) * 3 / 10; minKeep > keepCount {
		keepCount = minKeep
	}

	// Use the shared safeSplitIndex helper so the boundary semantics are
	// identical to maybeSummarize / TruncateBefore — no tool_call/tool_result
	// orphans on either side of the cut.
	splitIdx := safeSplitIndex(messages, len(messages)-keepCount)
	if splitIdx <= 1 {
		return nil
	}

	// Build summary input via the tool-collapse helper so tool invocations and
	// their (truncated) results survive into the summary — previously these
	// were silently dropped, causing the agent to forget what tools ran.
	toSummarize := messages[:splitIdx]
	flatHistory := collapseToolCallsForSummary(toSummarize)

	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Summarization model: default is to reuse the agent's primary model
	// (l.model), keeping summary quality consistent with the rest of the turn.
	// Operators can override via CompactionConfig.SummarizerModel (e.g. "fast")
	// to flip summarization to a cheaper model without a code change.
	summarizerModel := l.model
	if l.compactionCfg != nil && l.compactionCfg.SummarizerModel != "" {
		summarizerModel = l.compactionCfg.SummarizerModel
	} else if config.DefaultSummarizerModelAlias != "" {
		summarizerModel = config.DefaultSummarizerModelAlias
	}

	resp, err := l.provider.Chat(sctx, providers.ChatRequest{
		Messages: []providers.Message{{
			Role:    "user",
			Content: compactionSummaryPrompt + flatHistory,
		}},
		Model:   summarizerModel,
		Options: map[string]any{"max_tokens": 1024, "temperature": 0.3},
	})
	if err != nil {
		slog.Warn("mid_loop_compaction_failed", "agent", l.id, "error", err)
		return nil
	}

	// Collect MediaRefs from compacted messages (keep up to 30 most recent).
	const maxPreservedMediaRefs = 30
	var preservedRefs []providers.MediaRef
	for i := len(toSummarize) - 1; i >= 0 && len(preservedRefs) < maxPreservedMediaRefs; i-- {
		for _, ref := range toSummarize[i].MediaRefs {
			preservedRefs = append(preservedRefs, ref)
			if len(preservedRefs) >= maxPreservedMediaRefs {
				break
			}
		}
	}

	summary := providers.Message{
		Role:      "user",
		Content:   "[Summary of earlier conversation]\n" + SanitizeAssistantContent(resp.Content),
		MediaRefs: preservedRefs,
	}
	result := make([]providers.Message, 0, 1+keepCount)
	result = append(result, summary)
	result = append(result, messages[splitIdx:]...)

	slog.Info("mid_loop_compacted",
		"agent", l.id,
		"original_msgs", len(messages),
		"summarized", splitIdx,
		"kept", len(result))

	return result
}
