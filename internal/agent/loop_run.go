package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/internal/tracing"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// Run processes a single message through the agent loop.
// It blocks until completion and returns the final response.
func (l *Loop) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	l.activeRuns.Add(1)
	defer l.activeRuns.Add(-1)

	// Per-run emit wrapper: enriches every AgentEvent with delegation + routing context.
	emitRun := func(event AgentEvent) {
		event.RunKind = req.RunKind
		event.DelegationID = req.DelegationID
		event.TeamID = req.TeamID
		event.TeamTaskID = req.TeamTaskID
		event.ParentAgentID = req.ParentAgentID
		event.UserID = req.UserID
		event.Channel = req.Channel
		event.ChatID = req.ChatID
		event.SessionKey = req.SessionKey
		event.TenantID = store.TenantIDFromContext(ctx)
		l.emit(event)
	}

	emitRun(AgentEvent{
		Type:    protocol.AgentEventRunStarted,
		AgentID: l.id,
		RunID:   req.RunID,
		Payload: map[string]any{"message": req.Message},
	})

	// Create trace
	var traceID uuid.UUID
	isChildTrace := req.ParentTraceID != uuid.Nil && l.traceCollector != nil

	// agentSpanID holds the pre-generated root agent span ID.
	// Used by emitAgentSpanEnd in the deferred finalizer below.
	var agentSpanID uuid.UUID

	if isChildTrace {
		// Announce run: reuse parent trace, don't create new trace record.
		// Spans will be added to the parent trace with proper nesting.
		traceID = req.ParentTraceID
		ctx = tracing.WithTraceID(ctx, traceID)
		ctx = tracing.WithCollector(ctx, l.traceCollector)
		agentSpanID = store.GenNewID()
		ctx = tracing.WithParentSpanID(ctx, agentSpanID)
		if req.ParentRootSpanID != uuid.Nil {
			ctx = tracing.WithAnnounceParentSpanID(ctx, req.ParentRootSpanID)
		}
	} else if l.traceCollector != nil {
		traceID = store.GenNewID()
		now := time.Now().UTC()
		traceName := "chat " + l.id
		if req.TraceName != "" {
			traceName = req.TraceName
		}
		trace := &store.TraceData{
			ID:           traceID,
			RunID:        req.RunID,
			SessionKey:   req.SessionKey,
			UserID:       req.UserID,
			Channel:      req.Channel,
			Name:         traceName,
			InputPreview: truncateStr(req.Message, l.traceCollector.PreviewMaxLen()),
			Status:       store.TraceStatusRunning,
			StartTime:    now,
			CreatedAt:    now,
			Tags:         req.TraceTags,
		}
		if l.agentUUID != uuid.Nil {
			trace.AgentID = &l.agentUUID
		}
		// Link to parent trace: delegation context or explicit LinkedTraceID (team task runs).
		if delegateParent := tracing.DelegateParentTraceIDFromContext(ctx); delegateParent != uuid.Nil {
			trace.ParentTraceID = &delegateParent
		} else if req.LinkedTraceID != uuid.Nil {
			trace.ParentTraceID = &req.LinkedTraceID
		}
		// Set team_id on trace for team-scoped runs.
		if req.TeamID != "" {
			if tid, err := uuid.Parse(req.TeamID); err == nil {
				trace.TeamID = &tid
			}
		}
		if err := l.traceCollector.CreateTrace(ctx, trace); err != nil {
			slog.Warn("tracing: failed to create trace", "error", err)
		} else {
			ctx = tracing.WithTraceID(ctx, traceID)
			ctx = tracing.WithCollector(ctx, l.traceCollector)
			if trace.TeamID != nil {
				ctx = tracing.WithTraceTeamID(ctx, *trace.TeamID)
			}

			// Notify the gateway so it can associate this traceID with the active run
			// entry for force-abort (forceMarkTraceAborted needs traceID at abort time).
			if req.OnTraceCreated != nil {
				req.OnTraceCreated(traceID)
			}

			// Pre-generate root "agent" span ID so LLM/tool spans can reference it as parent.
			agentSpanID = store.GenNewID()
			ctx = tracing.WithParentSpanID(ctx, agentSpanID)
		}
	}

	// Inject local key into tool context so delegation/subagent tools can
	// propagate topic/thread routing info back through announce messages.
	if req.LocalKey != "" {
		ctx = tools.WithToolLocalKey(ctx, req.LocalKey)
	}

	runStart := time.Now().UTC()

	// Safety net: ensure root traces are ALWAYS finalized, even on panic or goroutine leak.
	// Normal-path finalization sets traceFinalized=true; this defer only acts if it wasn't.
	var traceFinalized bool
	if !isChildTrace && l.traceCollector != nil && traceID != uuid.Nil {
		defer func() {
			if traceFinalized {
				return
			}
			slog.Warn("tracing: safety-net finalizing orphan trace",
				"trace_id", traceID, "agent", l.id, "session", req.SessionKey)
			safeCtx := context.WithoutCancel(ctx)
			if agentSpanID != uuid.Nil {
				l.emitAgentSpanEnd(safeCtx, agentSpanID, runStart, nil, context.Canceled)
			}
			l.traceCollector.FinishTrace(safeCtx, traceID, store.TraceStatusError,
				"trace finalized by safety net (likely panic or goroutine leak)", "")
		}()
	}

	// Emit running agent span immediately so it's visible in the trace UI.
	if agentSpanID != uuid.Nil {
		var agentSpanOpts []spanOption
		if req.ModelOverride != "" {
			agentSpanOpts = append(agentSpanOpts, withModel(req.ModelOverride))
		}
		if req.ProviderOverride != nil {
			agentSpanOpts = append(agentSpanOpts, withProvider(req.ProviderOverride.Name()))
		}
		l.emitAgentSpanStart(ctx, agentSpanID, runStart, req.Message, agentSpanOpts...)
	}

	// Child trace (announce run): set parent trace back to "running" while
	// this run is active so the trace UI doesn't show "completed" with a
	// "running" child span.
	if isChildTrace && l.traceCollector != nil && traceID != uuid.Nil {
		l.traceCollector.SetTraceStatus(ctx, traceID, store.TraceStatusRunning)
	}

	// V3 pipeline path (always enabled).
	//
	// Barrier mode (when l.subagentMgr.BarrierMode() is true): we call the
	// pipeline up to `barrierMaxPasses + 1` times to drain spawned children
	// into the parent's run before finalizing. Pass 0 is the original user
	// request; passes 1+ inject a synthetic [System Message] block carrying
	// each batch of newly-completed subagent results. This replaces the
	// legacy announce-queue pseudo-run (separate RunID, separate stream) —
	// keeping everything under one RunID so the website's chunk handler,
	// Stop button, and resumable-stream replay all line up naturally.
	//
	// drainSpawnedChildren is nil-safe and returns drained=false on the
	// first call when no subagents were spawned, so the legacy path is the
	// fast default for prompts that don't use spawn at all.
	{
		var result *RunResult
		var err error
		consumedIDs := map[string]struct{}{}
		runReq := req
		for pass := 0; pass <= barrierMaxPasses; pass++ {
			var passResult *RunResult
			passResult, err = l.runViaPipeline(ctx, runReq)
			if err != nil {
				break
			}
			if pass == 0 {
				result = passResult
			} else if result != nil && passResult != nil {
				// Synthesis pass: take new content/thinking; sum usage
				// across passes so billing reflects total LLM work.
				result.Content = passResult.Content
				result.Thinking = passResult.Thinking
				result.Iterations += passResult.Iterations
				if len(passResult.Media) > 0 {
					result.Media = append(result.Media, passResult.Media...)
				}
				if passResult.Usage != nil {
					if result.Usage == nil {
						u := *passResult.Usage
						result.Usage = &u
					} else {
						result.Usage.PromptTokens += passResult.Usage.PromptTokens
						result.Usage.CompletionTokens += passResult.Usage.CompletionTokens
						result.Usage.TotalTokens += passResult.Usage.TotalTokens
						result.Usage.CacheCreationTokens += passResult.Usage.CacheCreationTokens
						result.Usage.CacheReadTokens += passResult.Usage.CacheReadTokens
					}
				}
			}
			if pass >= barrierMaxPasses {
				break
			}
			systemMsg, newConsumed, drained := l.drainSpawnedChildren(ctx, consumedIDs)
			if !drained {
				break
			}
			for _, id := range newConsumed {
				consumedIDs[id] = struct{}{}
			}
			logBarrierPass(req.RunID, pass+1, len(newConsumed), len(consumedIDs))
			// Next-pass request: same SessionKey + RunID + tenancy; just
			// swap the user message for the synthetic [System Message] block
			// and suppress its persistence as a visible user-role row.
			runReq = req
			runReq.Message = systemMsg
			runReq.HideInput = true
			runReq.Media = nil
			runReq.ForwardMedia = nil
		}
		// Tracing + events handled below via the same finalize path
		if err != nil {
			if agentSpanID != uuid.Nil {
				l.emitAgentSpanEnd(ctx, agentSpanID, runStart, nil, err)
			}
			if isChildTrace && l.traceCollector != nil && traceID != uuid.Nil {
				status := store.TraceStatusError
				if ctx.Err() != nil {
					status = store.TraceStatusCancelled
				}
				traceCtx := ctx
				if ctx.Err() != nil {
					traceCtx = context.WithoutCancel(ctx)
				}
				l.traceCollector.SetTraceStatus(traceCtx, traceID, status)
			}
			if ctx.Err() != nil {
				emitRun(AgentEvent{Type: protocol.AgentEventRunCancelled, AgentID: l.id, RunID: req.RunID})
			} else {
				emitRun(AgentEvent{Type: protocol.AgentEventRunFailed, AgentID: l.id, RunID: req.RunID, Payload: map[string]string{"error": err.Error()}})
			}
			if !isChildTrace && l.traceCollector != nil && traceID != uuid.Nil {
				traceFinalized = true
				traceCtx := ctx
				traceStatus := store.TraceStatusError
				if ctx.Err() != nil {
					traceCtx = context.WithoutCancel(ctx)
					traceStatus = store.TraceStatusCancelled
				}
				l.traceCollector.FinishTrace(traceCtx, traceID, traceStatus, err.Error(), "")
			}
			return nil, err
		}
		// Structured performance log for v3 pipeline runs.
		elapsed := time.Since(runStart)
		logAttrs := []any{
			"agent", l.id, "duration_ms", elapsed.Milliseconds(),
			"iterations", result.Iterations,
		}
		if result.Usage != nil {
			logAttrs = append(logAttrs, "total_tokens", result.Usage.TotalTokens)
		}
		slog.Info("v3.run.completed", logAttrs...)

		if agentSpanID != uuid.Nil {
			l.emitAgentSpanEnd(ctx, agentSpanID, runStart, result, nil)
		}
		if isChildTrace && l.traceCollector != nil && traceID != uuid.Nil {
			l.traceCollector.SetTraceStatus(ctx, traceID, store.TraceStatusCompleted)
		}
		completedPayload := map[string]any{"content": result.Content}
		if result.Thinking != "" {
			completedPayload["thinking"] = result.Thinking
		}
		if result != nil && result.Usage != nil {
			completedPayload["usage"] = map[string]any{
				"prompt_tokens":         result.Usage.PromptTokens,
				"completion_tokens":     result.Usage.CompletionTokens,
				"total_tokens":          result.Usage.TotalTokens,
				"cache_creation_tokens": result.Usage.CacheCreationTokens,
				"cache_read_tokens":     result.Usage.CacheReadTokens,
			}
		}
		if result != nil && len(result.Media) > 0 {
			completedPayload["media"] = result.Media
		}
		emitRun(AgentEvent{Type: protocol.AgentEventRunCompleted, AgentID: l.id, RunID: req.RunID, Payload: completedPayload})
		if !isChildTrace && l.traceCollector != nil && traceID != uuid.Nil {
			traceFinalized = true
			if result != nil {
				l.traceCollector.FinishTrace(ctx, traceID, store.TraceStatusCompleted, "", truncateStr(result.Content, l.traceCollector.PreviewMaxLen()))
			} else {
				l.traceCollector.FinishTrace(ctx, traceID, store.TraceStatusCompleted, "", "")
			}
		}
		return result, nil
	}
}
