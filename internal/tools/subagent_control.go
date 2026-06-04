package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// scheduleArchive removes a task after the archive TTL.
func (sm *SubagentManager) scheduleArchive(taskID string, after time.Duration) {
	time.Sleep(after)
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if t, ok := sm.tasks[taskID]; ok && t.Status != TaskStatusRunning {
		delete(sm.tasks, taskID)
		slog.Debug("subagent archived", "id", taskID)
	}
}

// GetTask returns a task by ID.
func (sm *SubagentManager) GetTask(id string) (*SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	t, ok := sm.tasks[id]
	return t, ok
}

// ListTasks returns all tasks, optionally filtered by parent.
func (sm *SubagentManager) ListTasks(parentID string) []*SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	var result []*SubagentTask
	for _, t := range sm.tasks {
		if parentID == "" || t.ParentID == parentID {
			result = append(result, t)
		}
	}
	return result
}

// SubagentSnapshotView is a value-copy of an in-memory SubagentTask,
// safe to read after the SubagentManager mutex is released. Used by
// the chat.activeSessions handler to enrich the WS snapshot with live
// subagent state (text + thinking + tool history) — so the SPA can
// rehydrate the nested mini-chat on page reload without waiting for
// new live events. Persistent state (after the task completes) is
// served by sessions.preview's collectSubagentsForHistory; this view
// covers the gap while the subagent is still streaming.
type SubagentSnapshotView struct {
	ID                string
	ParentToolCallID  string
	Label             string
	Task              string
	Model             string
	Status            string
	Result            string
	Thinking          string
	ToolHistory       []SubagentToolRecord
	TotalInputTokens  int64
	TotalOutputTokens int64
}

// SnapshotsByParentToolCallID returns a value-copy snapshot of every
// in-memory task that has a parent tool_call.id, keyed by that id.
// Cheap: O(N) over the in-memory task map under a read lock; N is
// bounded by the per-process active-tasks budget (default 4 × per-run
// children, archived after ArchiveAfterMinutes). Tasks without a
// ParentToolCallID (sync-run paths without a UI subscriber) are
// skipped — there's no chip to attach them to on the client.
func (sm *SubagentManager) SnapshotsByParentToolCallID() map[string]SubagentSnapshotView {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make(map[string]SubagentSnapshotView, len(sm.tasks))
	for _, t := range sm.tasks {
		if t.ParentToolCallID == "" {
			continue
		}
		view := SubagentSnapshotView{
			ID:                t.ID,
			ParentToolCallID:  t.ParentToolCallID,
			Label:             t.Label,
			Task:              t.Task,
			Model:             t.Model,
			Status:            t.Status,
			Result:            t.Result,
			Thinking:          t.Thinking,
			TotalInputTokens:  t.TotalInputTokens,
			TotalOutputTokens: t.TotalOutputTokens,
		}
		if len(t.ToolHistory) > 0 {
			view.ToolHistory = append(view.ToolHistory[:0:0], t.ToolHistory...)
		}
		out[t.ParentToolCallID] = view
	}
	return out
}

// CancelTask cancels a running task by ID.
// Special IDs: "all" cancels all running tasks for any parent,
// "last" cancels the most recently created running task.
func (sm *SubagentManager) CancelTask(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if id == "all" {
		cancelled := false
		for _, t := range sm.tasks {
			if t.Status == TaskStatusRunning {
				sm.cancelTaskLocked(t)
				cancelled = true
			}
		}
		return cancelled
	}

	if id == "last" {
		var latest *SubagentTask
		for _, t := range sm.tasks {
			if t.Status == TaskStatusRunning {
				if latest == nil || t.CreatedAt > latest.CreatedAt {
					latest = t
				}
			}
		}
		if latest == nil {
			return false
		}
		sm.cancelTaskLocked(latest)
		return true
	}

	t, ok := sm.tasks[id]
	if !ok || t.Status != TaskStatusRunning {
		return false
	}
	sm.cancelTaskLocked(t)
	return true
}

// CancelTasksForParent cancels all running tasks for a specific parent.
func (sm *SubagentManager) CancelTasksForParent(parentID string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	count := 0
	for _, t := range sm.tasks {
		if t.ParentID == parentID && t.Status == TaskStatusRunning {
			sm.cancelTaskLocked(t)
			count++
		}
	}
	return count
}

// cancelTaskLocked sets a task to cancelled and fires its context cancel.
// Must be called with sm.mu held.
func (sm *SubagentManager) cancelTaskLocked(t *SubagentTask) {
	t.Status = TaskStatusCancelled
	t.Result = "cancelled by user"
	t.CompletedAt = time.Now().UnixMilli()
	if t.cancelFunc != nil {
		t.cancelFunc()
	}
}

// Steer cancels a running subagent and restarts it with a new message.
// Matching TS subagents-tool.ts steer action: cancel → settle → spawn replacement.
func (sm *SubagentManager) Steer(
	ctx context.Context,
	taskID, newMessage string,
	callback AsyncCallback,
) (string, error) {
	sm.mu.Lock()
	t, ok := sm.tasks[taskID]
	if !ok {
		sm.mu.Unlock()
		return "", fmt.Errorf("subagent %q not found", taskID)
	}
	if t.Status != TaskStatusRunning {
		sm.mu.Unlock()
		return "", fmt.Errorf("subagent %q is not running (status=%s)", taskID, t.Status)
	}

	// Capture origin metadata before cancelling
	parentID := t.ParentID
	depth := t.Depth - 1 // Spawn increments depth, so use original
	label := t.Label + " (steered)"
	model := t.Model
	channel := t.OriginChannel
	chatID := t.OriginChatID
	peerKind := t.OriginPeerKind

	// Cancel old task (suppress announce by marking cancelled before unlock)
	sm.cancelTaskLocked(t)
	sm.mu.Unlock()

	// Brief settle period (matching TS 500ms settle)
	time.Sleep(500 * time.Millisecond)

	// Truncate message to 4000 chars (matching TS MAX_STEER_MESSAGE_LENGTH)
	if len(newMessage) > 4000 {
		newMessage = newMessage[:4000]
	}

	// Spawn replacement
	msg, err := sm.Spawn(ctx, parentID, depth, newMessage, label, model,
		channel, chatID, peerKind, callback)
	if err != nil {
		return "", fmt.Errorf("steer respawn failed: %w", err)
	}

	return fmt.Sprintf("Steered subagent %q → new task spawned. %s", taskID, msg), nil
}

// WaitForChildren blocks until all running tasks for parentID complete or timeout.
func (sm *SubagentManager) WaitForChildren(ctx context.Context, parentID string, timeoutSec int) ([]*SubagentTask, error) {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	deadline := time.After(time.Duration(timeoutSec) * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return sm.ListTasks(parentID), ctx.Err()
		case <-deadline:
			return sm.ListTasks(parentID), fmt.Errorf("timeout after %ds waiting for children", timeoutSec)
		case <-ticker.C:
			tasks := sm.ListTasks(parentID)
			allDone := true
			for _, t := range tasks {
				if t.Status == TaskStatusRunning {
					allDone = false
					break
				}
			}
			if allDone {
				return tasks, nil
			}
		}
	}
}

// ListTasksByRunID returns every task spawned under the given agent run.
// Mirrors ListTasks(parentID) but matches the structured-concurrency
// scope: one Loop.Run owns its children, so parallel runs on the same
// agent (multi-chat per user) don't interfere with each other's
// barriers. Empty runID returns no tasks — callers should fall back to
// ListTasks(parentID) when the run id isn't known (announce / cron /
// HTTP callers that don't propagate ctxToolRunID).
func (sm *SubagentManager) ListTasksByRunID(runID string) []*SubagentTask {
	if runID == "" {
		return nil
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	var result []*SubagentTask
	for _, t := range sm.tasks {
		if t.ParentRunID == runID {
			result = append(result, t)
		}
	}
	return result
}

// WaitForChildrenByRunID is the run-scoped variant of WaitForChildren.
// Used by the agent loop's pre-finalize barrier so a parent run waits
// for ONLY the subagents it itself spawned, not every task under the
// same parent agent. Without this scope two parallel chats on the
// same agent share a global task list and each chat's barrier
// blocks on the other chat's children. Same poll cadence + timeout
// semantics as the legacy WaitForChildren — only the task filter
// differs.
func (sm *SubagentManager) WaitForChildrenByRunID(ctx context.Context, runID string, timeoutSec int) ([]*SubagentTask, error) {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	deadline := time.After(time.Duration(timeoutSec) * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return sm.ListTasksByRunID(runID), ctx.Err()
		case <-deadline:
			return sm.ListTasksByRunID(runID), fmt.Errorf("timeout after %ds waiting for children", timeoutSec)
		case <-ticker.C:
			tasks := sm.ListTasksByRunID(runID)
			allDone := true
			for _, t := range tasks {
				if t.Status == TaskStatusRunning {
					allDone = false
					break
				}
			}
			if allDone {
				return tasks, nil
			}
		}
	}
}

func generateSubagentID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "sub-" + hex.EncodeToString(b)
}

func truncate(s string, maxLen int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxLen {
		return s
	}
	// Don't cut in the middle of a multi-byte rune
	for maxLen > 0 && !utf8.RuneStart(s[maxLen]) {
		maxLen--
	}
	return s[:maxLen] + "..."
}
