package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// toolResultAction describes what the caller should do after processing a tool result.
type toolResultAction int

const (
	toolResultContinue toolResultAction = iota // proceed normally
	toolResultWarning                          // injected warning message, continue
	toolResultBreak                            // critical loop detected, break iteration
)

// processToolResult handles post-execution bookkeeping for a single tool result:
// loop detection, event emission, media collection, deliverables, and message building.
// Used by both single-tool and parallel-tool paths to eliminate duplication.
//
// Returns the tool message, an optional warning message to inject, and an action signal.
// The caller must append toolMsg and warningMsg to messages/pendingMsgs, and break if action == toolResultBreak.
func (l *Loop) processToolResult(
	ctx context.Context,
	rs *runState,
	req *RunRequest,
	emitRun func(AgentEvent),
	tc providers.ToolCall,
	registryName string,
	result *tools.Result,
	hadBootstrap bool,
) (toolMsg providers.Message, warningMsgs []providers.Message, action toolResultAction) {

	// Record for loop detection.
	argsHash := rs.loopDetector.record(registryName, tc.Arguments)
	rs.loopDetector.recordResult(argsHash, result.ForLLM)
	rs.loopDetector.recordMutation(registryName, tc.Arguments)

	if result.Async {
		rs.asyncToolCalls = append(rs.asyncToolCalls, tc.Name)
	}

	if result.IsError {
		errMsg := result.ForLLM
		if len(errMsg) > 200 {
			errMsg = errMsg[:200] + "..."
		}
		slog.Warn("tool error", "agent", l.id, "tool", tc.Name, "error", errMsg)
	}

	// Count successful spawn calls for orphan detection (post-execution).
	if registryName == "spawn" && !result.IsError {
		if tid, _ := tc.Arguments["team_task_id"].(string); tid != "" {
			rs.teamTaskSpawns++
		}
	}
	if hadBootstrap && bootstrapToolAllowlist[registryName] {
		rs.bootstrapWriteDetected = true
	}

	// Emit tool result event.
	toolResultPayload := map[string]any{
		"name":      tc.Name,
		"id":        tc.ID,
		"is_error":  result.IsError,
		"arguments": tc.Arguments,
		"result":    truncateStr(result.ForLLM, 1000),
	}
	if result.IsError && result.ForLLM != "" {
		toolResultPayload["content"] = result.ForLLM
	}
	// Live media attach: when the tool produced files (write_file,
	// create_pdf, create_image, …) ship them on this event so the SPA
	// can render attachment chips on the streaming bubble immediately.
	// Without this, files only appear after run.completed when the saved
	// message's media_refs gets loaded — which is why a mid-stream chat
	// shows "wrote 3 files" in text but no actual download buttons until
	// you reload the page (the load path reads from sessions.preview).
	// Paths are signed to /v1/files/...?ft=... here so the SPA can hit
	// them with no extra round-trip — same shape sessions.preview emits.
	if live := buildLiveMediaPayload(result.Media); live != nil {
		toolResultPayload["media"] = live
	}
	emitRun(AgentEvent{
		Type:    protocol.AgentEventToolResult,
		AgentID: l.id,
		RunID:   req.RunID,
		Payload: toolResultPayload,
	})

	l.scanWebToolResult(tc.Name, result)

	// Collect MEDIA: paths from tool results.
	// Prefer result.Media (explicit) over ForLLM MEDIA: prefix (legacy) to avoid duplicates.
	if len(result.Media) > 0 {
		for _, mf := range result.Media {
			ct := mf.MimeType
			if ct == "" {
				ct = mimeFromExt(filepath.Ext(mf.Path))
			}
			rs.mediaResults = append(rs.mediaResults, MediaResult{
				Path:        mf.Path,
				ContentType: ct,
				Filename:    mf.Filename,
			})
		}
	} else if mr := parseMediaResult(result.ForLLM); mr != nil {
		rs.mediaResults = append(rs.mediaResults, *mr)
	}
	// Auto-attach workspace media to task (covers create_image/audio/video).
	if teamWs := tools.ToolTeamWorkspaceFromCtx(ctx); teamWs != "" {
		for _, mf := range result.Media {
			tools.AutoAttachWorkspaceFile(ctx, l.teamStore, teamWs, mf.Path)
		}
	}
	if result.Deliverable != "" {
		rs.deliverables = append(rs.deliverables, result.Deliverable)
	}

	toolMsg = providers.Message{
		Role:       "tool",
		Content:    result.ForLLM,
		ToolCallID: tc.ID,
		IsError:    result.IsError,
	}

	action = toolResultContinue

	// Check for tool call loop after recording result.
	if level, msg := rs.loopDetector.detect(registryName, argsHash); level != "" {
		if level == "critical" {
			slog.Warn("tool loop critical", "agent", l.id, "tool", registryName, "message", msg)
			rs.finalContent = "I had to stop — I kept repeating the same step" +
				stuckToolDetail(registryName, tc.Arguments) +
				" without making progress." + stuckResultDetail(result) +
				"\n\nTell me how you'd like to proceed (or fix the issue above) and I'll continue."
			rs.loopKilled = true
			return toolMsg, nil, toolResultBreak
		}
		slog.Warn("tool loop warning", "agent", l.id, "tool", registryName, "message", msg)
		warningMsgs = append(warningMsgs, providers.Message{Role: "user", Content: msg})
		action = toolResultWarning
	}

	// Check for same tool returning identical results with different args.
	if rh := hashResult(result.ForLLM); rh != "" {
		if level, msg := rs.loopDetector.detectSameResult(registryName, rh); level != "" {
			if level == "critical" {
				slog.Warn("tool loop critical: same result",
					"tool", registryName, "agent", l.id, "run", req.RunID, "detail", msg)
				// Don't surface the raw "CRITICAL ... runaway loop" debug string to
				// the user (the same-args path above uses a friendly message too).
				// Any artifact already produced is still attached via deliverables.
				rs.finalContent = "I've stopped because the same step kept producing the same result without moving forward" +
					stuckToolDetail(registryName, tc.Arguments) + "." + stuckResultDetail(result) +
					"\n\nIf that's something you can fix (a permission, setting, or missing detail), let me know — otherwise tell me what to do differently."
				rs.loopKilled = true
				return toolMsg, nil, toolResultBreak
			}
			warningMsgs = append(warningMsgs, providers.Message{Role: "user", Content: msg})
			action = toolResultWarning
		}
	}

	return toolMsg, warningMsgs, action
}

// stuckToolDetail renders a short, user-facing description of what a tool was
// repeatedly doing, appended to loop-stop messages so the user can see what got
// stuck (and supply a missing value) instead of a generic "I repeated a step".
// Returns "" when there's nothing specific worth surfacing.
// jsonDetailRe pulls a human-readable message out of a JSON error body
// (e.g. X's {"detail":"You are not permitted to perform this action."}).
var jsonDetailRe = regexp.MustCompile(`"(?:detail|message|error|error_description)"\s*:\s*"([^"]{1,400})"`)

// stuckResultDetail surfaces the ACTUAL tool output that kept repeating, so the
// stop message tells the user the real problem (e.g. an X "not permitted" error)
// instead of guessing. Returns "" when there's nothing useful to show.
func stuckResultDetail(result *tools.Result) string {
	if result == nil {
		return ""
	}
	msg := cleanToolResultForUser(result.ForLLM)
	if msg == "" {
		return ""
	}
	if result.IsError {
		return "\n\nThe tool reported this error:\n  " + msg
	}
	return "\n\nThe tool kept returning:\n  " + msg
}

// cleanToolResultForUser turns a raw tool result into a concise, user-facing
// snippet: drops the untrusted-content wrapper, prefers a JSON detail/message/
// error field, collapses whitespace, and truncates. Credentials are already
// scrubbed upstream (registry.ScrubCredentials).
func cleanToolResultForUser(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "<<<EXTERNAL_UNTRUSTED_CONTENT>>>"); i >= 0 {
		s = s[i+len("<<<EXTERNAL_UNTRUSTED_CONTENT>>>"):]
	}
	if m := jsonDetailRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	s = strings.Join(strings.Fields(s), " ") // collapse newlines/whitespace
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func stuckToolDetail(toolName string, args map[string]any) string {
	if toolName == "execute_action" {
		action, _ := args["action"].(string)
		selector, _ := args["selector"].(string)
		switch {
		case action != "" && selector != "":
			return fmt.Sprintf(" (repeatedly trying to %s `%s`)", action, selector)
		case selector != "":
			return fmt.Sprintf(" (repeatedly acting on `%s`)", selector)
		}
	}
	return ""
}

// checkReadOnlyStreak detects when the agent is stuck in a read-only loop.
// Returns warning messages to inject and whether the loop should break.
func (l *Loop) checkReadOnlyStreak(rs *runState, req *RunRequest) (warningMsg *providers.Message, shouldBreak bool) {
	level, msg := rs.loopDetector.detectReadOnlyStreak()
	if level == "" {
		return nil, false
	}
	if level == "critical" {
		slog.Warn("tool loop critical: read-only streak",
			"streak", rs.loopDetector.readOnlyStreak,
			"unique", rs.loopDetector.readOnlyUnique,
			"agent", l.id, "run", req.RunID)
		rs.finalContent = msg
		rs.loopKilled = true
		return nil, true
	}
	slog.Warn("tool loop warning: read-only streak",
		"streak", rs.loopDetector.readOnlyStreak, "agent", l.id, "run", req.RunID)
	warnMsg := providers.Message{Role: "user", Content: msg}
	return &warnMsg, false
}
