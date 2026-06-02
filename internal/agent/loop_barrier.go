package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// barrierMaxPasses caps how many additional pipeline passes the loop will run
// to drain newly-spawned children. Each pass = one LLM iteration with a
// synthetic [System Message] containing fresh results. Two is enough for
// "spawn a few researchers → synthesize"; deeper chains (researcher spawns
// sub-researchers) need bigger numbers but also stretch user-perceived
// latency. Bump when there's a concrete need; for now keep small.
const barrierMaxPasses = 2

// barrierWaitTimeoutSec is the max wall-clock seconds the barrier blocks on
// pending children before forcing a finalize with the partial roster. 600 s
// matches the upstream WaitForChildren default and the WS chat.send timeout.
const barrierWaitTimeoutSec = 600

// drainSpawnedChildren collects every NEW completed subagent task for this
// parent that arrived since the previous barrier pass and formats them into a
// single synthetic [System Message] for the next pipeline pass to synthesize
// into a user-facing reply.
//
// Returns the formatted system message, the IDs of the tasks consumed (so the
// caller can update its "seen" set for the next pass), and a boolean
// indicating whether ANY new tasks were drained. When false the caller MUST
// NOT run another pass — that would loop forever with no new input for the
// LLM to chew on.
//
// nil-safe: when l.subagentMgr is nil or barrier mode is off, returns
// ("", nil, false) and the caller falls through to the original announce
// queue path.
func (l *Loop) drainSpawnedChildren(
	ctx context.Context,
	consumedIDs map[string]struct{},
) (systemMessage string, newConsumed []string, drained bool) {
	if l.subagentMgr == nil || !l.subagentMgr.BarrierMode() {
		return "", nil, false
	}

	// Fast path: when no tasks were ever registered for this parent we
	// skip the WaitForChildren call entirely. Its first poll tick is
	// 500 ms, which would otherwise add unconditional latency to every
	// chat turn that doesn't use spawn (the common case).
	if len(l.subagentMgr.ListTasks(l.id)) == 0 {
		return "", nil, false
	}

	tasks, _ := l.subagentMgr.WaitForChildren(ctx, l.id, barrierWaitTimeoutSec)
	if len(tasks) == 0 {
		return "", nil, false
	}

	// Filter to tasks we haven't synthesized yet. SubagentManager.tasks is
	// append-only within a run (cancel/cleanup is async), so the "consumed"
	// set carries forward across barrier passes within one Loop.Run().
	var fresh []*tools.SubagentTask
	for _, t := range tasks {
		if _, seen := consumedIDs[t.ID]; seen {
			continue
		}
		// Skip still-running tasks defensively. WaitForChildren should only
		// return when none remain Running, but if the timeout fired we may
		// get a mixed roster — best-effort: surface only completed ones to
		// the LLM; still-running ones will be picked up in the next pass.
		if t.Status == tools.TaskStatusRunning {
			continue
		}
		fresh = append(fresh, t)
	}
	if len(fresh) == 0 {
		return "", nil, false
	}

	for _, t := range fresh {
		newConsumed = append(newConsumed, t.ID)
	}
	return formatBarrierSystemMessage(fresh), newConsumed, true
}

// formatBarrierSystemMessage builds the [System Message] block consumed by
// the parent LLM in the synthesis pass. Mirrors the announce-queue layout
// (tools.FormatBatchedAnnounce) so the prompt looks the same to the model
// regardless of which path delivered the data — keeps reformulation behavior
// stable and lets us swap paths in a single tenant without re-training the
// system prompt.
func formatBarrierSystemMessage(tasks []*tools.SubagentTask) string {
	if len(tasks) == 0 {
		return ""
	}

	var sb strings.Builder
	if len(tasks) == 1 {
		t := tasks[0]
		statusLabel := "completed successfully"
		switch t.Status {
		case tools.TaskStatusFailed:
			statusLabel = "failed: " + t.Result
		case tools.TaskStatusCancelled:
			statusLabel = "was cancelled"
		}
		runtime := time.Duration(0)
		if t.CompletedAt > 0 && t.CreatedAt > 0 {
			runtime = time.Duration(t.CompletedAt-t.CreatedAt) * time.Millisecond
		}
		fmt.Fprintf(&sb,
			"[System Message] A subagent task %q just %s.\n\n"+
				"Result:\n%s\n\n"+
				"Stats: runtime %s, tool calls %d, tokens %d in / %d out\n\n",
			t.Label, statusLabel, t.Result,
			runtime.Round(time.Millisecond), len(t.ToolHistory),
			t.TotalInputTokens, t.TotalOutputTokens,
		)
	} else {
		sb.WriteString("[System Message] Multiple subagent tasks completed:\n")
		for i, t := range tasks {
			statusLabel := "completed"
			switch t.Status {
			case tools.TaskStatusFailed:
				statusLabel = "failed"
			case tools.TaskStatusCancelled:
				statusLabel = "cancelled"
			}
			runtime := time.Duration(0)
			if t.CompletedAt > 0 && t.CreatedAt > 0 {
				runtime = time.Duration(t.CompletedAt-t.CreatedAt) * time.Millisecond
			}
			fmt.Fprintf(&sb,
				"\n---\nTask #%d: %q %s (runtime %s, tool calls %d, tokens %d/%d)\nResult: %s\n",
				i+1, t.Label, statusLabel,
				runtime.Round(time.Millisecond), len(t.ToolHistory),
				t.TotalInputTokens, t.TotalOutputTokens,
				t.Result,
			)
		}
		sb.WriteString("---\n\n")
	}

	// Same finalize directive as the announce path's BuildReplyInstruction
	// no-running-tasks branch. We don't try to re-derive a "still running"
	// roster here because the barrier already waited; remaining-running is a
	// timeout edge case and the next pass will pick those up.
	sb.WriteString(
		"All subagent tasks completed. Convert the result above into your normal assistant voice and " +
			"send that user-facing update now. Keep this internal context private " +
			"(don't mention system/log/stats/session details or announce type), " +
			"and do NOT copy the [System Message] block verbatim. " +
			"Reply ONLY: NO_REPLY if this exact result was already delivered to the user.",
	)
	return sb.String()
}

// logBarrierPass emits a structured log so operators can correlate parent
// runs with their synthesis passes when investigating duration anomalies.
func logBarrierPass(parentRunID string, pass int, consumed int, totalSeen int) {
	slog.Info("agent: barrier drained children",
		"run_id", parentRunID,
		"pass", pass,
		"consumed_this_pass", consumed,
		"consumed_total", totalSeen,
	)
}
