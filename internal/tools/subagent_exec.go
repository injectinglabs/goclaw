package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tracing"
)

// truncateStrSub trims a string to maxLen runes, appending "…" if it was
// cut. Used for live-progress event payloads so a chatty tool result
// (a 50k web_fetch dump) doesn't fill the WS buffer. The full result
// still flows through the existing ToolHistory + announce paths.
func truncateStrSub(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// formatSubagentToolHistory renders the subagent's tool calls as a compact
// Markdown table so the parent UI can show what the child did without a
// custom protocol — the result string round-trips through MessageContent
// (markdown renderer) on the frontend. Returns an empty string when the
// history is empty (subagent run never called any tools), so the existing
// callers don't have to special-case the zero-row table.
func formatSubagentToolHistory(history []SubagentToolRecord) string {
	if len(history) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nTools used:\n\n")
	b.WriteString("| # | Tool | Duration | Status |\n")
	b.WriteString("|---|------|----------|--------|\n")
	for i, rec := range history {
		statusIcon := "✓"
		if rec.Status == "error" {
			statusIcon = "✗"
		}
		dur := fmt.Sprintf("%dms", rec.DurationMs)
		if rec.DurationMs >= 1000 {
			dur = fmt.Sprintf("%.1fs", float64(rec.DurationMs)/1000.0)
		}
		fmt.Fprintf(&b, "| %d | `%s` | %s | %s |\n", i+1, rec.Name, dur, statusIcon)
	}
	return b.String()
}

// runTask executes the subagent in a goroutine.
func (sm *SubagentManager) runTask(ctx context.Context, task *SubagentTask, callback AsyncCallback) {
	iterations := sm.executeTask(ctx, task)

	// Announce result to parent via bus (matching TS subagent-announce.ts pattern).
	// The announce goes through the parent agent's session so the agent can
	// reformulate the result for the user.
	if sm.msgBus != nil && task.OriginChannel != "" {
		elapsed := time.Since(time.UnixMilli(task.CreatedAt))

		item := AnnounceQueueItem{
			SubagentID:   task.ID,
			Label:        task.Label,
			Status:       task.Status,
			Result:       task.Result,
			Media:        task.Media,
			Runtime:      elapsed,
			Iterations:   iterations,
			InputTokens:  task.TotalInputTokens,
			OutputTokens: task.TotalOutputTokens,
		}
		meta := AnnounceMetadata{
			OriginChannel:    task.OriginChannel,
			OriginChatID:     task.OriginChatID,
			OriginPeerKind:   task.OriginPeerKind,
			OriginLocalKey:   task.OriginLocalKey,
			OriginUserID:     task.OriginUserID,
			OriginSenderID:   task.OriginSenderID,
			OriginRole:       task.OriginRole,
			OriginSessionKey: task.OriginSessionKey,
			OriginTenantID:   task.OriginTenantID,
			ParentAgent:      task.ParentID,
			OriginTraceID:    task.OriginTraceID.String(),
			OriginRootSpanID: task.OriginRootSpanID.String(),
		}

		if sm.announceQueue != nil {
			// Use batched announce queue (matching TS debounce pattern)
			sessionKey := fmt.Sprintf("announce:%s:%s", task.ParentID, task.OriginChatID)
			sm.announceQueue.Enqueue(sessionKey, item, meta)
		} else {
			// Direct publish (no batching)
			roster := sm.RosterForParent(task.ParentID)
			announceContent := FormatBatchedAnnounce([]AnnounceQueueItem{item}, roster)

			announceMeta := map[string]string{
				MetaOriginChannel:      task.OriginChannel,
				MetaOriginPeerKind:     task.OriginPeerKind,
				MetaParentAgent:        task.ParentID,
				"subagent_id":          task.ID,
				MetaSubagentLabel:      task.Label,
				MetaSubagentStatus:     task.Status,
				MetaSubagentResult:     task.Result,
				MetaSubagentRuntime:    fmt.Sprintf("%d", elapsed.Milliseconds()),
				MetaSubagentIterations: fmt.Sprintf("%d", iterations),
				MetaSubagentInputToks:  fmt.Sprintf("%d", task.TotalInputTokens),
				MetaSubagentOutputToks: fmt.Sprintf("%d", task.TotalOutputTokens),
				MetaOriginTraceID:      task.OriginTraceID.String(),
				MetaOriginRootSpanID:   task.OriginRootSpanID.String(),
			}
			if task.OriginLocalKey != "" {
				announceMeta[MetaOriginLocalKey] = task.OriginLocalKey
			}
			if task.OriginSessionKey != "" {
				announceMeta[MetaOriginSessionKey] = task.OriginSessionKey
			}
			if task.OriginSenderID != "" {
				announceMeta[MetaOriginSenderID] = task.OriginSenderID
			}
			if task.OriginRole != "" {
				announceMeta[MetaOriginRole] = task.OriginRole
			}
			if task.OriginUserID != "" {
				announceMeta[MetaOriginUserID] = task.OriginUserID
			}
			sm.msgBus.PublishInbound(bus.InboundMessage{
				Channel:  "system",
				SenderID: fmt.Sprintf("subagent:%s", task.ID),
				ChatID:   task.OriginChatID,
				Content:  announceContent,
				UserID:   task.OriginUserID,
				TenantID: task.OriginTenantID,
				Metadata: announceMeta,
				Media:    task.Media,
			})
		}
	}

	// Call completion callback
	if callback != nil {
		// Prepend a compact "Tools used" timeline before the final result
		// text so the parent's UI (which renders the spawn tool call's
		// result through MessageContent markdown) shows what the
		// subagent actually did — instead of a single opaque blob.
		// Skipped when the run made zero tool calls (pure text turn).
		toolsBlock := formatSubagentToolHistory(task.ToolHistory)
		result := NewResult(fmt.Sprintf("Subagent '%s' completed in %d iterations.%s\n\nResult:\n%s",
			task.Label, iterations, toolsBlock, task.Result))
		callback(ctx, result)
	}
}

// executeTask runs the LLM tool loop for a subagent. Returns iteration count.
func (sm *SubagentManager) executeTask(ctx context.Context, task *SubagentTask) int {
	// Diagnostic: log the values our live-progress WS events depend on.
	// If either is empty/nil at this point, the event guards in the
	// iteration loop will silently skip emission and the website never
	// receives the events that nest tool.call/tool.result under the
	// parent's spawn chip. Without this log we couldn't tell from prod
	// logs whether the spawn-tool ctx was missing the values, or the
	// goroutine had them but the WS filter dropped them.
	slog.Info("subagent runtask start",
		"id", task.ID,
		"label", task.Label,
		"parent_tool_call_id", task.ParentToolCallID,
		"has_emit_event", task.emitEvent != nil)

	// Tracing: generate a root span ID for this subagent execution.
	// LLM/tool spans will nest under this root span via parent_span_id.
	// The root span itself links to the parent agent's root span (from ctx).
	subRootSpanID := store.GenNewID()
	taskStart := time.Now().UTC()

	// Use a detached context for tracing so spans are emitted even if parent ctx is cancelled.
	// We copy tracing values but remove the cancellation chain.
	traceCtx := context.Background()
	if collector := tracing.CollectorFromContext(ctx); collector != nil {
		traceCtx = tracing.WithCollector(traceCtx, collector)
		traceCtx = tracing.WithTraceID(traceCtx, tracing.TraceIDFromContext(ctx))
		// Keep original parent_span_id (parent agent's root span) for the subagent root span.
		traceCtx = tracing.WithParentSpanID(traceCtx, tracing.ParentSpanIDFromContext(ctx))
	}

	// subCtx overrides parent_span_id so child spans nest under subRootSpanID.
	// traceCtx retains the original parent_span_id for the root subagent span.
	subTraceCtx := tracing.WithParentSpanID(traceCtx, subRootSpanID)

	var model string
	var finalContent string
	iteration := 0

	defer func() {
		sm.mu.Lock()
		task.CompletedAt = time.Now().UnixMilli()
		sm.mu.Unlock()

		// Finalize root subagent span on exit (uses traceCtx which is never cancelled).
		sm.emitSubagentSpanEnd(traceCtx, subRootSpanID, taskStart, task, finalContent)
		slog.Debug("subagent tracing: root span finalized",
			"id", task.ID, "span_id", subRootSpanID,
			"trace_id", tracing.TraceIDFromContext(traceCtx),
			"status", task.Status, "iterations", iteration)

		// Schedule auto-archive
		if task.spawnConfig.ArchiveAfterMinutes > 0 {
			go sm.scheduleArchive(task.ID, time.Duration(task.spawnConfig.ArchiveAfterMinutes)*time.Minute)
		}
	}()

	if ctx.Err() != nil {
		sm.mu.Lock()
		task.Status = TaskStatusCancelled
		task.Result = "cancelled before execution"
		sm.mu.Unlock()
		return 0
	}

	// Build tools for subagent (no spawn/subagent tools to prevent recursion)
	toolsReg := sm.createTools()
	sm.applyDenyList(toolsReg, task.Depth, task.spawnConfig)

	// Determine model (cascading priority):
	// 1. Per-task model override (highest — LLM specified model in spawn call)
	// 2. SubagentConfig.Model (agent-level subagent override)
	// 3. Parent agent's model (inherit from the agent that spawned us)
	// 4. SubagentManager default model (system-wide fallback)
	model = sm.model
	if parentModel := ParentModelFromCtx(ctx); parentModel != "" {
		model = parentModel
	}
	if task.spawnConfig.Model != "" {
		model = task.spawnConfig.Model
	}
	if task.Model != "" {
		model = task.Model
	}

	// Determine provider (cascading priority):
	// 1. Parent agent's provider (inherit so model/provider combo stays valid)
	// 2. SubagentManager default provider (system-wide fallback)
	activeProvider := sm.provider
	if sm.providerReg != nil {
		if parentProviderName := ParentProviderFromCtx(ctx); parentProviderName != "" {
			if p, err := sm.providerReg.Get(ctx, parentProviderName); err == nil {
				activeProvider = p
			}
		}
	}
	// Multi-tenant deployments may construct SubagentManager with a nil
	// sm.provider (cmd/gateway_agents.setupSubagents — providers are registered
	// per-tenant, so a tenant-less startup lookup returns nil). In that case
	// we MUST find a per-tenant provider via ParentProviderFromCtx above;
	// otherwise activeProvider stays nil and `activeProvider.Name()` below
	// would NPE inside the subagent execution path. Bail gracefully with a
	// task-error result so the parent agent can recover instead of crashing
	// the loop.
	if activeProvider == nil {
		sm.mu.Lock()
		task.Status = TaskStatusFailed
		task.Result = "subagent: no provider available — tenant context missing or provider not registered for this tenant"
		sm.mu.Unlock()
		return 0
	}

	// Emit running subagent root span (after model resolution so span has correct model).
	sm.emitSubagentSpanStart(traceCtx, subRootSpanID, taskStart, task, model, activeProvider.Name())

	// Tell the UI a subagent run is starting so it can render an empty mini-
	// chat bubble immediately, even before the first text chunk arrives.
	// Without this the spawn chip stays in its "accepted" placeholder state
	// for the whole first iteration (often a few seconds) while the model
	// generates its plan — the user sees a frozen indicator and assumes
	// nothing's happening.
	if task.emitEvent != nil && task.ParentToolCallID != "" {
		task.emitEvent("subagent.run.started", map[string]any{
			"parent_tool_call_id": task.ParentToolCallID,
			"subagent_id":         task.ID,
			"subagent_label":      task.Label,
			"task":                task.Task,
			"model":               model,
		})
	}

	// Build subagent system prompt (matching TS buildSubagentSystemPrompt pattern).
	workspace := ToolWorkspaceFromCtx(ctx)
	systemPrompt := sm.buildSubagentSystemPrompt(task, task.spawnConfig, workspace)

	messages := []providers.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task.Task},
	}

	// Run LLM iteration loop (similar to agent loop but simplified)
	var mediaFiles []bus.MediaFile
	maxIterations := 20

	for iteration < maxIterations {
		iteration++

		if ctx.Err() != nil {
			sm.mu.Lock()
			task.Status = TaskStatusCancelled
			task.Result = "cancelled during execution"
			sm.mu.Unlock()
			return iteration
		}

		chatReq := providers.ChatRequest{
			Messages: messages,
			Tools:    toolsReg.ProviderDefs(),
			Model:    model,
			Options: map[string]any{
				"max_tokens":  4096,
				"temperature": 0.5,
			},
		}

		llmStart := time.Now().UTC()
		llmSpanID := sm.emitLLMSpanStart(subTraceCtx, llmStart, iteration, model, activeProvider.Name(), messages)

		maxRetries := task.spawnConfig.MaxRetries
		if maxRetries <= 0 {
			maxRetries = 2
		}
		var resp *providers.ChatResponse
		var err error
		for attempt := 0; attempt <= maxRetries; attempt++ {
			if attempt > 0 {
				backoff := time.Duration(attempt) * 2 * time.Second
				select {
				case <-ctx.Done():
				case <-time.After(backoff):
				}
				if ctx.Err() != nil {
					break
				}
				slog.Info("subagent LLM retry", "id", task.ID, "iteration", iteration, "attempt", attempt+1)
			}
		// ctx is the parent agent's run context — cancelling the parent (e.g. agent abort)
		// cascades here and to all subsequent tool calls in this iteration.
		// Do NOT replace ctx with context.Background() here; that would detach abort propagation.
		//
		// Stream the LLM reply when the parent has a WS subscription so the
		// nested mini-chat gets text token-by-token — same UX as the
		// parent bubble. Each delta becomes a subagent.chunk event tagged
		// with parent_tool_call_id + subagent_id.
		//
		// On error, log enough context to diagnose if anything upstream
		// (NGINX/WAF/ALB) rejects subagent streaming for any reason —
		// without this we couldn't tell stage 403 issues apart from
		// quota/timeout/auth failures.
			if task.emitEvent != nil && task.ParentToolCallID != "" {
				resp, err = activeProvider.ChatStream(ctx, chatReq, func(chunk providers.StreamChunk) {
					if chunk.Content == "" && chunk.Thinking == "" {
						return
					}
					payload := map[string]any{
						"parent_tool_call_id": task.ParentToolCallID,
						"subagent_id":         task.ID,
						"subagent_label":      task.Label,
						"iteration":           iteration,
					}
					if chunk.Content != "" {
						payload["content"] = chunk.Content
					}
					if chunk.Thinking != "" {
						payload["thinking"] = chunk.Thinking
					}
					task.emitEvent("subagent.chunk", payload)
				})
				if err != nil {
					// Diagnostic: log request shape so we can compare against parent's
					// successful ChatStream calls if stage rejects something subagent-
					// specific. msgCount + lastRole help spot history-size differences;
					// hasTools + provider gives the auth/path context.
					lastRole := ""
					if len(chatReq.Messages) > 0 {
						lastRole = chatReq.Messages[len(chatReq.Messages)-1].Role
					}
					slog.Warn("subagent ChatStream error",
						"id", task.ID,
						"iteration", iteration,
						"provider", activeProvider.Name(),
						"model", model,
						"msgCount", len(chatReq.Messages),
						"lastRole", lastRole,
						"hasTools", len(chatReq.Tools) > 0,
						"error", err)
				}
			} else {
				resp, err = activeProvider.Chat(ctx, chatReq)
			}
			if err == nil {
				break
			}
		}

		sm.emitLLMSpanEnd(subTraceCtx, llmSpanID, llmStart, resp, err)

		// Accumulate token usage for cost tracking.
		if resp != nil && resp.Usage != nil {
			sm.mu.Lock()
			task.TotalInputTokens += int64(resp.Usage.PromptTokens)
			task.TotalOutputTokens += int64(resp.Usage.CompletionTokens)
			sm.mu.Unlock()
		}

		if err != nil {
			sm.mu.Lock()
			task.Status = TaskStatusFailed
			task.Result = fmt.Sprintf("LLM error at iteration %d: %v", iteration, err)
			sm.mu.Unlock()
			slog.Warn("subagent LLM error", "id", task.ID, "iteration", iteration, "error", err)
			go sm.persistStatus(ctx, task, iteration)
			return iteration
		}

		// No tool calls → done
		if len(resp.ToolCalls) == 0 {
			finalContent = resp.Content
			break
		}

		// Build assistant message
		assistantMsg := providers.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// Execute tools
		for _, tc := range resp.ToolCalls {
			slog.Debug("subagent tool call", "id", task.ID, "tool", tc.Name)

			argsJSON, _ := json.Marshal(tc.Arguments)
			toolStart := time.Now().UTC()
			toolSpanID := sm.emitToolSpanStart(subTraceCtx, toolStart, tc.Name, tc.ID, string(argsJSON))

			// LIVE progress: emit tool.call on the parent run's WS subscription,
			// tagged with parent_tool_call_id + subagent_id so the website
			// routes it under the right spawn chip and renders a running
			// indicator. emitEvent is captured from the spawn context — nil
			// when the subagent runs from a sync path (RunSync) that has no
			// UI subscriber.
			if task.emitEvent != nil && task.ParentToolCallID != "" {
				task.emitEvent("tool.call", map[string]any{
					"name":                 tc.Name,
					"id":                   tc.ID,
					"arguments":            tc.Arguments,
					"parent_tool_call_id":  task.ParentToolCallID,
					"subagent_id":          task.ID,
					"subagent_label":       task.Label,
				})
			}

			result := toolsReg.Execute(ctx, tc.Name, tc.Arguments)
			sm.emitToolSpanEnd(subTraceCtx, toolSpanID, toolStart, result.ForLLM, result.IsError)

			// Track this tool call in the task's history so the announce
			// callback can surface "what the subagent actually did" back to
			// the parent's UI without subscribing to per-call WS events.
			// Status = "error" if the tool returned an is_error result,
			// "ok" otherwise. Duration is wall-clock ms across Execute().
			recStatus := "ok"
			if result.IsError {
				recStatus = "error"
			}
			sm.mu.Lock()
			task.ToolHistory = append(task.ToolHistory, SubagentToolRecord{
				Name:       tc.Name,
				DurationMs: time.Since(toolStart).Milliseconds(),
				Status:     recStatus,
			})
			sm.mu.Unlock()

			// LIVE progress: emit tool.result so the website flips the
			// nested chip from running to done/error in real time.
			if task.emitEvent != nil && task.ParentToolCallID != "" {
				task.emitEvent("tool.result", map[string]any{
					"name":                tc.Name,
					"id":                  tc.ID,
					"is_error":            result.IsError,
					"result":              truncateStrSub(result.ForLLM, 2000),
					"parent_tool_call_id": task.ParentToolCallID,
					"subagent_id":         task.ID,
					"subagent_label":      task.Label,
				})
			}

			// Capture media file paths from tool results (e.g. image generation).
			if len(result.Media) > 0 {
				mediaFiles = append(mediaFiles, result.Media...)
			} else if strings.HasPrefix(strings.TrimSpace(result.ForLLM), "MEDIA:") {
				// Fallback: parse MEDIA: prefix from ForLLM (same as agent loop's parseMediaResult)
				p := strings.TrimSpace(strings.TrimSpace(result.ForLLM)[6:])
				if nl := strings.IndexByte(p, '\n'); nl >= 0 {
					p = strings.TrimSpace(p[:nl])
				}
				if p != "" {
					mediaFiles = append(mediaFiles, bus.MediaFile{Path: p, Filename: filepath.Base(p)})
				}
			}

			messages = append(messages, providers.Message{
				Role:       "tool",
				Content:    result.ForLLM,
				ToolCallID: tc.ID,
			})
		}
	}

	// Last-chance synthesis: if the iteration loop terminated without the
	// model ever producing a tool-call-free text turn (it kept calling
	// tools, then either hit the max-iterations cap or returned empty
	// content), give it ONE more shot — same conversation, tools stripped
	// from the request, plus an explicit nudge to write the deliverable
	// as text. This catches the common failure mode where the subagent
	// writes findings to a file with write_file and exits silently: the
	// "Task completed but no final response was generated" fallback was
	// being used unnecessarily because the model COULD have answered, it
	// just didn't realise it was supposed to.
	if finalContent == "" && activeProvider != nil {
		messages = append(messages, providers.Message{
			Role: "user",
			Content: "Now write the complete deliverable as your final response — plain text only, no tool calls. The parent agent only sees this text; anything you put in files is unreachable to it.",
		})
		finalReq := providers.ChatRequest{
			Model:    model,
			Messages: messages,
			// Explicitly omit Tools so the model can ONLY emit text.
		}
		if resp, err := activeProvider.Chat(ctx, finalReq); err == nil && resp != nil && resp.Content != "" {
			finalContent = resp.Content
			if resp.Usage != nil {
				sm.mu.Lock()
				task.TotalInputTokens += int64(resp.Usage.PromptTokens)
				task.TotalOutputTokens += int64(resp.Usage.CompletionTokens)
				sm.mu.Unlock()
			}
			slog.Info("subagent last-chance synthesis recovered output",
				"id", task.ID, "chars", len(finalContent))
		}
	}

	sm.mu.Lock()
	if finalContent == "" {
		finalContent = "Task completed but no final response was generated."
	}
	task.Status = TaskStatusCompleted
	task.Result = finalContent
	task.Media = mediaFiles
	sm.mu.Unlock()

	slog.Info("subagent completed", "id", task.ID, "iterations", iteration)

	// Tell the UI the subagent run is done so it can flip the nested
	// mini-chat bubble from "streaming" to "done", show the final
	// markdown table from ToolHistory, and stop the typing dots. Fires
	// regardless of completion status (success / failed / cancelled —
	// the status string lets the UI pick the right styling).
	if task.emitEvent != nil && task.ParentToolCallID != "" {
		task.emitEvent("subagent.run.completed", map[string]any{
			"parent_tool_call_id": task.ParentToolCallID,
			"subagent_id":         task.ID,
			"subagent_label":      task.Label,
			"status":              task.Status,
			"content":             task.Result,
			"iterations":          iteration,
			"input_tokens":        task.TotalInputTokens,
			"output_tokens":       task.TotalOutputTokens,
		})
	}

	// Persist final status to DB (fire-and-forget).
	go sm.persistStatus(ctx, task, iteration)

	return iteration
}
