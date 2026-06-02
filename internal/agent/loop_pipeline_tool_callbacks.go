package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// makeExecuteToolCall wraps tool execution: name resolution, execute, process result.
// Uses bridgeRS to share loop detection state between the pipeline and agent's processToolResult.
func (l *Loop) makeExecuteToolCall(req *RunRequest, bridgeRS *runState) func(ctx context.Context, state *pipeline.RunState, tc providers.ToolCall) ([]providers.Message, error) {
	emitRun := makeToolEmitRun(l, req)
	return func(ctx context.Context, state *pipeline.RunState, tc providers.ToolCall) ([]providers.Message, error) {
		registryName := l.resolveToolCallName(tc.Name)
		argsJSON, _ := json.Marshal(tc.Arguments)
		slog.Info("tool call", "agent", l.id, "tool", tc.Name, "args_len", len(argsJSON))

		emitRun(AgentEvent{
			Type:    protocol.AgentEventToolCall,
			AgentID: l.id,
			RunID:   state.RunID,
			Payload: map[string]any{"name": tc.Name, "id": tc.ID, "arguments": tc.Arguments},
		})

		// Emit tool span start for tracing.
		toolStart := time.Now().UTC()
		toolSpanID := l.emitToolSpanStart(ctx, toolStart, tc.Name, tc.ID, string(argsJSON))

		// Inject agent audio snapshot so TTS tool (and any future audio consumers)
		// can read agent-level voice/model config without an extra DB lookup.
		if l.agentUUID != uuid.Nil {
			ctx = store.WithAgentAudio(ctx, store.AgentAudioSnapshot{
				AgentID:     l.agentUUID,
				OtherConfig: append([]byte(nil), l.agentOtherConfig...), // defensive copy at dispatch
			})
		}

		var result *tools.Result
		if l.registry != nil && l.registry.GetMetadata(registryName).IsClient {
			// Client tool: dispatch to browser extension, block on result channel.
			result = l.dispatchClientTool(ctx, req, emitRun, tc)
		} else {
			// asyncCB: when an async tool (spawn) eventually completes, fire a
			// second `tool.result` event with the same toolCall.id so the
			// website's expand-chip swaps the immediate accepted-response for
			// the real deliverable + tool history. Upstream wires this to nil
			// — there's no canonical channel between SubagentManager.runTask
			// and the parent's run-event stream, so the subagent's announce
			// only re-enters as an inbound system message. Result: the chip
			// never updates and the user sees only "accepted, started..."
			// forever. Fork-local fix: closure over emitRun + tc.ID so the
			// re-emit lands on the same UI element.
			asyncCB := l.makeAsyncToolCallback(req, emitRun, tc)
			// Live subagent progress: stamp the spawn tool_call.id +
			// event emitter onto context so the SubagentManager can
			// emit tool.call / tool.result events on this same run's
			// WS subscription, tagged with parent_tool_call_id so the
			// website routes them to the right spawn chip. See
			// internal/tools/context_keys.go for the keys and
			// subagent_exec.go's tool loop for the emit sites.
			execCtx := tools.WithParentToolCallID(ctx, tc.ID)
			execCtx = tools.WithToolEventEmitter(execCtx, l.makeToolEventEmitterForRun(req))
			result = l.tools.ExecuteWithContext(execCtx, registryName, tc.Arguments,
				req.Channel, req.ChatID, req.PeerKind, req.SessionKey, asyncCB)
		}
		toolDuration := time.Since(toolStart)

		l.emitToolSpanEnd(ctx, toolSpanID, toolStart, result)

		// v3 evolution metrics: record tool execution non-blocking (best-effort).
		l.recordToolMetric(ctx, req.SessionKey, registryName, !result.IsError, toolDuration)

		toolMsg, warningMsgs, action := l.processToolResult(ctx, bridgeRS, req, emitRun, tc, registryName, result, state.Context.HadBootstrap)
		syncBridgeToState(bridgeRS, state, action)

		var msgs []providers.Message
		msgs = append(msgs, toolMsg)
		msgs = append(msgs, warningMsgs...)
		return msgs, nil
	}
}

// toolRawResult wraps a tools.Result with timing for metrics recording.
type toolRawResult struct {
	result   *tools.Result
	duration time.Duration
}

// makeExecuteToolRaw wraps tool I/O only (parallel-safe, no state mutation).
// Returns tool message + toolRawResult (with timing + spanID) as opaque raw data for ProcessToolResult.
func (l *Loop) makeExecuteToolRaw(req *RunRequest) func(ctx context.Context, tc providers.ToolCall) (providers.Message, any, error) {
	emitRun := makeToolEmitRun(l, req)
	return func(ctx context.Context, tc providers.ToolCall) (providers.Message, any, error) {
		registryName := l.resolveToolCallName(tc.Name)
		argsJSON, _ := json.Marshal(tc.Arguments)
		slog.Info("tool call", "agent", l.id, "tool", tc.Name, "args_len", len(argsJSON))

		// Emit tool.call event at I/O start — parity with sequential path (makeExecuteToolCall).
		// Without this, parallel tool execution (2+ concurrent tools) never notifies UI of
		// tool invocation, so `tool.result` arrives with no matching `tool.call` to update.
		// Bus.Broadcast is RWMutex-guarded; safe to call from parallel goroutines.
		emitRun(AgentEvent{
			Type:    protocol.AgentEventToolCall,
			AgentID: l.id,
			RunID:   req.RunID,
			Payload: map[string]any{"name": tc.Name, "id": tc.ID, "arguments": tc.Arguments},
		})

		// Emit tool span start (goroutine-safe: channel send only).
		start := time.Now().UTC()
		spanID := l.emitToolSpanStart(ctx, start, tc.Name, tc.ID, string(argsJSON))

		// Inject agent audio snapshot (parallel path — same as sequential makeExecuteToolCall).
		if l.agentUUID != uuid.Nil {
			ctx = store.WithAgentAudio(ctx, store.AgentAudioSnapshot{
				AgentID:     l.agentUUID,
				OtherConfig: append([]byte(nil), l.agentOtherConfig...), // defensive copy at dispatch
			})
		}

		var result *tools.Result
		if l.registry != nil && l.registry.GetMetadata(registryName).IsClient {
			// Client tool: dispatch to browser extension, block on result channel.
			// Parallel path — still one goroutine per call, so blocking here is fine.
			result = l.dispatchClientTool(ctx, req, emitRun, tc)
		} else {
			// See sequential-path comment above for why asyncCB is non-nil here.
			asyncCB := l.makeAsyncToolCallback(req, emitRun, tc)
			// Same live-progress ctx stamping as the sequential path.
			execCtx := tools.WithParentToolCallID(ctx, tc.ID)
			execCtx = tools.WithToolEventEmitter(execCtx, l.makeToolEventEmitterForRun(req))
			result = l.tools.ExecuteWithContext(execCtx, registryName, tc.Arguments,
				req.Channel, req.ChatID, req.PeerKind, req.SessionKey, asyncCB)
		}
		dur := time.Since(start)

		// Emit tool span end inside goroutine to prevent orphaned spans on ctx cancellation.
		l.emitToolSpanEnd(ctx, spanID, start, result)

		msg := providers.Message{
			Role:       "tool",
			Content:    result.ForLLM,
			ToolCallID: tc.ID,
			IsError:    result.IsError,
		}
		return msg, &toolRawResult{result: result, duration: dur}, nil
	}
}

// makeProcessToolResult wraps post-execution bookkeeping (sequential, mutates bridgeRS).
// rawData is *toolRawResult from ExecuteToolRaw — no re-execution.
func (l *Loop) makeProcessToolResult(req *RunRequest, bridgeRS *runState) func(ctx context.Context, state *pipeline.RunState, tc providers.ToolCall, rawMsg providers.Message, rawData any) []providers.Message {
	emitRun := makeToolEmitRun(l, req)
	return func(ctx context.Context, state *pipeline.RunState, tc providers.ToolCall, rawMsg providers.Message, rawData any) []providers.Message {
		registryName := l.resolveToolCallName(tc.Name)

		// Extract result and timing from toolRawResult wrapper.
		var result *tools.Result
		var dur time.Duration
		if raw, ok := rawData.(*toolRawResult); ok && raw != nil {
			result = raw.result
			dur = raw.duration
		} else if r, ok := rawData.(*tools.Result); ok {
			result = r // backward compat
		}
		if result == nil {
			return []providers.Message{rawMsg}
		}

		// Record tool metrics (non-blocking, best-effort).
		l.recordToolMetric(ctx, req.SessionKey, registryName, !result.IsError, dur)

		toolMsg, warningMsgs, action := l.processToolResult(ctx, bridgeRS, req, emitRun, tc, registryName, result, state.Context.HadBootstrap)
		syncBridgeToState(bridgeRS, state, action)

		var msgs []providers.Message
		msgs = append(msgs, toolMsg)
		msgs = append(msgs, warningMsgs...)
		return msgs
	}
}

// makeCheckReadOnly wraps read-only streak detection using the bridged runState.
func (l *Loop) makeCheckReadOnly(req *RunRequest, bridgeRS *runState) func(state *pipeline.RunState) (*providers.Message, bool) {
	return func(state *pipeline.RunState) (*providers.Message, bool) {
		warnMsg, shouldBreak := l.checkReadOnlyStreak(bridgeRS, req)
		if shouldBreak {
			state.Tool.LoopKilled = bridgeRS.loopKilled
			state.Observe.FinalContent = bridgeRS.finalContent
		}
		return warnMsg, shouldBreak
	}
}

// syncBridgeToState copies side effects from bridgeRS to pipeline RunState.
func syncBridgeToState(bridgeRS *runState, state *pipeline.RunState, action toolResultAction) {
	state.Tool.LoopKilled = bridgeRS.loopKilled
	state.Tool.AsyncToolCalls = bridgeRS.asyncToolCalls
	state.Tool.Deliverables = bridgeRS.deliverables
	state.Evolution.BootstrapWrite = bridgeRS.bootstrapWriteDetected
	state.Evolution.TeamTaskSpawns = bridgeRS.teamTaskSpawns
	state.Evolution.TeamTaskCreates = bridgeRS.teamTaskCreates
	// Sync media results from v2 processToolResult → v3 pipeline state.
	// Without this, MEDIA: paths from tool results never reach FinalizeStage.
	if len(bridgeRS.mediaResults) > 0 {
		state.Tool.MediaResults = state.Tool.MediaResults[:0]
		for _, mr := range bridgeRS.mediaResults {
			state.Tool.MediaResults = append(state.Tool.MediaResults, pipeline.MediaResult{
				Path:        mr.Path,
				ContentType: mr.ContentType,
				Size:        mr.Size,
				AsVoice:     mr.AsVoice,
				Filename:    mr.Filename,
			})
		}
	}
	if state.Tool.LoopKilled && action == toolResultBreak {
		state.Observe.FinalContent = bridgeRS.finalContent
	}
}

// recordToolMetric records a tool execution metric non-blocking (best-effort).
// No-op when evolution metrics store is not configured.
func (l *Loop) recordToolMetric(ctx context.Context, sessionKey, toolName string, success bool, duration time.Duration) {
	if l.evolutionMetricsStore == nil {
		return
	}
	tenantID := store.TenantIDFromContext(ctx)
	go func() {
		bgCtx, cancel := context.WithTimeout(store.WithTenantID(context.Background(), tenantID), 5*time.Second)
		defer cancel()
		value, _ := json.Marshal(map[string]any{
			"success":     success,
			"duration_ms": duration.Milliseconds(),
		})
		if err := l.evolutionMetricsStore.RecordMetric(bgCtx, store.EvolutionMetric{
			ID:         uuid.New(),
			TenantID:   tenantID,
			AgentID:    l.agentUUID,
			SessionKey: sessionKey,
			MetricType: store.MetricTool,
			MetricKey:  toolName,
			Value:      value,
		}); err != nil {
			slog.Debug("evolution.metric.record_failed", "tool", toolName, "error", err)
		}
	}()
}

// makeToolEventEmitterForRun returns a tools.ToolEventEmitter that
// publishes events on the parent run's WS subscription. Used by
// SubagentManager to surface child tool.call/tool.result events
// in real time under the parent's spawn chip on the UI.
//
// The agent package owns AgentEvent + protocol types; tools is a
// lower layer and can't import them (would cycle). So the emitter
// signature is plain (string, map[string]any). AgentEvent.Type is just
// a string (see loop_types.go) — protocol.AgentEventToolCall et al.
// are untyped string constants — so we pass the eventType straight
// through with no cast.
func (l *Loop) makeToolEventEmitterForRun(req *RunRequest) tools.ToolEventEmitter {
	emitRun := makeToolEmitRun(l, req)
	return func(eventType string, payload map[string]any) {
		emitRun(AgentEvent{
			Type:    eventType,
			AgentID: l.id,
			RunID:   req.RunID,
			Payload: payload,
		})
	}
}

// makeAsyncToolCallback returns an AsyncCallback that re-emits a
// `tool.result` event for the SAME toolCall.id once an async tool
// (currently only `spawn`) finishes. The website's WS handler matches
// `tool.result` events to existing toolCall chips by id, so the
// expand body swaps the "accepted, started…" placeholder for the
// real deliverable (which for subagents includes the Markdown tool
// history table seeded by the announce callback in subagent_exec.go).
//
// Upstream goclaw passes nil here — there's no canonical bridge
// from SubagentManager.runTask back to the parent's tool-event
// channel, so the chip never updates and the user sees only the
// immediate accepted response forever. Fork-local fix.
//
// Truncation cap is 8000 chars so the table + a few hundred chars of
// subagent body comfortably fit (the immediate-result path uses 1000
// chars which is too tight for the full deliverable).
func (l *Loop) makeAsyncToolCallback(req *RunRequest, emitRun func(AgentEvent), tc providers.ToolCall) tools.AsyncCallback {
	return func(_ context.Context, result *tools.Result) {
		if result == nil {
			return
		}
		payload := map[string]any{
			"name":     tc.Name,
			"id":       tc.ID,
			"is_error": result.IsError,
			"result":   truncateStr(result.ForLLM, 8000),
		}
		if result.ForLLM != "" {
			// Frontend prefers `content` when present (sync path uses the
			// same key for the unsanitised full result); keep both for
			// symmetry so the chip's body shows the full deliverable.
			payload["content"] = result.ForLLM
		}
		// Mirror the live-media attach the sync tool-result path does
		// (loop_tools.go). For spawn this is the canonical "subagent
		// produced N files" delivery — parent's nested chip updates to
		// include the actual download buttons instead of just text.
		if len(result.Media) > 0 {
			live := make([]map[string]string, 0, len(result.Media))
			for _, mf := range result.Media {
				ct := mf.MimeType
				if ct == "" {
					ct = mimeFromExt(filepath.Ext(mf.Path))
				}
				live = append(live, map[string]string{
					"path":      mf.Path,
					"filename":  mf.Filename,
					"mime_type": ct,
				})
			}
			payload["media"] = live
		}
		emitRun(AgentEvent{
			Type:    protocol.AgentEventToolResult,
			AgentID: l.id,
			RunID:   req.RunID,
			Payload: payload,
		})
	}
}

// makeToolEmitRun creates a tool event emitter with request context.
// TenantID is critical: clientCanReceiveEvent fail-closes events with
// zero tenant for non-owner WS clients, silently dropping tool.call,
// tool.result, and client_tool_call events if it isn't set.
func makeToolEmitRun(l *Loop, req *RunRequest) func(AgentEvent) {
	return func(event AgentEvent) {
		event.RunKind = req.RunKind
		event.SessionKey = req.SessionKey
		event.UserID = req.UserID
		event.Channel = req.Channel
		event.TenantID = l.tenantID
		l.emit(event)
	}
}
