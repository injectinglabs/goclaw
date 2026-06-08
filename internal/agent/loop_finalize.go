package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"log/slog"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// isUserFilePopulated checks if USER.md has been filled with actual user data
// beyond the blank template. The template has "- **Name:**\n" with no value.
func isUserFilePopulated(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	// Template markers: "**Name:**" followed by newline (no value) or just whitespace
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "- **Name:**" || line == "**Name:**" {
			return false // name field still empty
		}
	}
	return true
}

// finalizeRun performs post-loop processing: sanitization, media dedup, session flush,
// bootstrap cleanup, and builds the final RunResult.
func (l *Loop) finalizeRun(
	ctx context.Context,
	rs *runState,
	req *RunRequest,
	history []providers.Message,
	hadBootstrap bool,
	toolTiming ToolTimingMap,
) *RunResult {
	// 5. Full sanitization pipeline (matching TS extractAssistantText + sanitizeUserFacingText)
	rs.finalContent = SanitizeAssistantContent(rs.finalContent)

	// 6. Handle NO_REPLY: save to session for context but mark as silent.
	isSilent := IsSilentReply(rs.finalContent)

	// 5b. Skill evolution: postscript suggestion after complex tasks.
	if l.skillEvolve && l.skillNudgeInterval > 0 &&
		rs.totalToolCalls >= l.skillNudgeInterval &&
		rs.finalContent != "" && !isSilent && !rs.skillPostscriptSent {
		rs.skillPostscriptSent = true
		locale := store.LocaleFromContext(ctx)
		rs.finalContent += "\n\n---\n_" + i18n.T(locale, i18n.MsgSkillNudgePostscript) + "_"
	}

	// 7. Fallback for empty content.
	//
	// Upstream behaviour was to emit a literal "..." whenever the model
	// ended its turn without producing text — usually after a tool-call
	// round where the model fetched data but forgot to summarise. Before
	// giving up we try a single lightweight rescue: call the provider
	// once with tools disabled and an explicit "finalise please" nudge,
	// so the model is forced to produce text using the data already in
	// history. Keeps the call cheap (no tool dispatch, no iteration) and
	// handles the most common empty-reply case without pipeline
	// surgery. A full retry-with-tools (where the model might call more
	// tools) is still on the backlog — that needs the pipeline to
	// re-execute a stage, which this file can't do.
	if rs.finalContent == "" {
		// Stream the rescue retry's tokens back over the same WS subscription
		// the parent run is using, so the UI bubble fills live instead of
		// staying empty until reload pulls the saved finalContent from DB.
		emitChunk := l.makeRescueChunkEmitter(req)
		if rescued := l.rescueEmptyReply(ctx, history, emitChunk); rescued != "" {
			rs.finalContent = SanitizeAssistantContent(rescued)
		}
	}

	// If even the rescue returned nothing, fall back to a localised
	// sentence the user can act on — replaces the legacy "..." placeholder.
	if rs.finalContent == "" {
		locale := store.LocaleFromContext(ctx)
		rs.finalContent = i18n.T(locale, i18n.MsgEmptyReplyFallback)
		slog.Info("agent: empty assistant content, using localised fallback",
			"locale", locale,
			"hadAsyncToolCalls", len(rs.asyncToolCalls) > 0)
	}

	// Append content suffix (e.g. image markdown for WS) before saving to session.
	// Dedup by basename: skip suffix lines whose file already appears in the agent's text.
	if req.ContentSuffix != "" {
		rs.finalContent += deduplicateMediaSuffix(rs.finalContent, req.ContentSuffix)
	}

	// Collect forwarded media + dedup + populate sizes BEFORE saving to session,
	// so we can attach output MediaRefs to the assistant message for history reload.
	for _, mf := range req.ForwardMedia {
		ct := mf.MimeType
		if ct == "" {
			ct = mimeFromExt(filepath.Ext(mf.Path))
		}
		rs.mediaResults = append(rs.mediaResults, MediaResult{Path: mf.Path, ContentType: ct})
	}
	rs.mediaResults = deduplicateMedia(rs.mediaResults)
	for i := range rs.mediaResults {
		if rs.mediaResults[i].Size == 0 {
			if info, err := os.Stat(rs.mediaResults[i].Path); err == nil {
				rs.mediaResults[i].Size = info.Size()
			}
		}
	}

	// Build final assistant message with output media refs for history persistence.
	// Usage attaches the run's cumulative token accounting so sessions.preview
	// can ship it back on reload — without this the SPA's per-bubble token
	// chip would only ever populate live (via run.completed event) and go
	// blank after a page reload. Subagent tokens already survive reload via
	// the subagent_tasks table; this closes the gap for the parent message.
	// Copy by value so future mutations on rs.totalUsage don't ripple back.
	usageCopy := rs.totalUsage
	assistantMsg := providers.Message{
		Role:     "assistant",
		Content:  rs.finalContent,
		Thinking: rs.finalThinking,
		Usage:    &usageCopy,
	}
	for _, mr := range rs.mediaResults {
		kind := "document"
		if strings.HasPrefix(mr.ContentType, "image/") {
			kind = "image"
		} else if strings.HasPrefix(mr.ContentType, "audio/") {
			kind = "audio"
		} else if strings.HasPrefix(mr.ContentType, "video/") {
			kind = "video"
		}
		assistantMsg.MediaRefs = append(assistantMsg.MediaRefs, providers.MediaRef{
			ID:       filepath.Base(mr.Path),
			MimeType: mr.ContentType,
			Kind:     kind,
			Path:     mr.Path,
			Filename: mr.Filename,
		})
	}
	rs.pendingMsgs = append(rs.pendingMsgs, assistantMsg)

	// Bootstrap nudge: if model didn't call write_file on turn 2+, inject reminder
	// into session history so the next turn sees it.
	if hadBootstrap && l.bootstrapCleanup != nil {
		nudgeUserTurns := 1
		for _, m := range history {
			if m.Role == "user" {
				nudgeUserTurns++
			}
		}
		if !rs.bootstrapWriteDetected && nudgeUserTurns >= 2 && nudgeUserTurns < bootstrapAutoCleanupTurns {
			rs.pendingMsgs = append(rs.pendingMsgs, providers.Message{
				Role:    "user",
				Content: "[System] You haven't completed onboarding yet. Please update USER.md with the user's details and clear BOOTSTRAP.md as instructed.",
			})
		}
	}

	// Bootstrap auto-cleanup: after enough conversation turns, remove BOOTSTRAP.md.
	// If USER.md is still the blank template, inject a reminder so the agent fills it.
	// Must run BEFORE session flush so the nudge message is persisted to history.
	if hadBootstrap && l.bootstrapCleanup != nil {
		userTurns := 1 // current user message
		for _, m := range history {
			if m.Role == "user" {
				userTurns++
			}
		}
		if userTurns >= bootstrapAutoCleanupTurns {
			if cleanErr := l.bootstrapCleanup(ctx, l.agentUUID, req.UserID); cleanErr != nil {
				slog.Warn("bootstrap auto-cleanup failed", "error", cleanErr, "agent", l.id, "user", req.UserID)
			} else {
				slog.Info("bootstrap auto-cleanup completed", "agent", l.id, "user", req.UserID, "turns", userTurns)
				// Check if USER.md is still the blank template — nudge agent to fill it
				if l.contextFileLoader != nil {
					files := l.contextFileLoader(ctx, l.agentUUID, req.UserID, l.agentType)
					for _, f := range files {
						if f.Path == bootstrap.UserFile && !isUserFilePopulated(f.Content) {
							rs.pendingMsgs = append(rs.pendingMsgs, providers.Message{
								Role:    "user",
								Content: "[System] You completed onboarding but USER.md is still empty. Please update USER.md with the user's name and details from this conversation using write_file.",
							})
							break
						}
					}
				}
			}
		}
	}

	// Flush all buffered messages to session atomically.
	for _, msg := range rs.pendingMsgs {
		l.sessions.AddMessage(ctx, req.SessionKey, msg)
	}

	// Persist adaptive tool timing to session metadata.
	if serialized := toolTiming.Serialize(); serialized != "" {
		l.sessions.SetSessionMetadata(ctx, req.SessionKey, map[string]string{"tool_timing": serialized})
	}

	// Write session metadata (matching TS session entry updates)
	l.sessions.UpdateMetadata(ctx, req.SessionKey, l.model, l.provider.Name(), req.Channel)
	l.sessions.AccumulateTokens(ctx, req.SessionKey, int64(rs.totalUsage.PromptTokens), int64(rs.totalUsage.CompletionTokens))

	// Calibrate token estimation: store the FINAL iteration's prompt_tokens
	// + message count. Using totalUsage.PromptTokens here would over-count
	// across tool-loop iterations and produce a misleading context-fill
	// indicator. rs.lastPromptTokens is the snapshot of the last call only.
	// (This v2 path is currently legacy / unused; the live pipeline path
	// goes through makeUpdateMetadata. Keep the two in sync defensively.)
	if rs.lastPromptTokens > 0 {
		msgCount := len(history) + rs.checkpointFlushedMsgs + len(rs.pendingMsgs)
		l.sessions.SetLastPromptTokens(ctx, req.SessionKey, rs.lastPromptTokens, msgCount)
	}

	l.sessions.Save(ctx, req.SessionKey)

	// 8. Metadata Stripping: Clean internal [[...]] tags for user-facing content
	rs.finalContent = StripMessageDirectives(rs.finalContent)
	if isSilent {
		slog.Info("agent loop: NO_REPLY detected, suppressing delivery",
			"agent", l.id, "session", req.SessionKey)
		rs.finalContent = ""
	}

	// 9. Maybe summarize
	l.maybeSummarize(ctx, req.SessionKey)

	// V3: emit session.completed for consolidation pipeline (episodic → semantic → dreaming)
	if l.domainBus != nil {
		// Unify user_id across channels. When a channel_contact is already
		// merged to a tenant_user via POST /v1/contacts/merge, this returns
		// that canonical tenant_users.user_id so Episodic/Semantic/Dreaming
		// workers write memory under one identity for the same human. Falls
		// back to req.UserID when no merge exists, preserving isolation for
		// unknown senders.
		memUserID := l.resolveCredentialUserID(ctx, *req)
		l.domainBus.Publish(eventbus.DomainEvent{
			Type:     eventbus.EventSessionCompleted,
			TenantID: l.tenantID.String(),
			AgentID:  l.agentUUID.String(),
			UserID:   memUserID,
			SourceID: req.SessionKey,
			Payload: &eventbus.SessionCompletedPayload{
				SessionKey:      req.SessionKey,
				MessageCount:    len(history) + len(rs.pendingMsgs),
				TokensUsed:      rs.totalUsage.PromptTokens + rs.totalUsage.CompletionTokens,
				CompactionCount: l.sessions.GetCompactionCount(ctx, req.SessionKey),
			},
		})
	}

	return &RunResult{
		Content:        rs.finalContent,
		Thinking:       rs.finalThinking,
		RunID:          req.RunID,
		Iterations:     rs.iteration,
		Usage:          &rs.totalUsage,
		Media:          rs.mediaResults,
		Deliverables:   rs.deliverables,
		BlockReplies:   rs.blockReplies,
		LastBlockReply: rs.lastBlockReply,
		LoopKilled:     rs.loopKilled,
	}
}

// rescueEmptyReply fires one provider call with tools disabled and a
// "finalise using the history above" nudge, to pull text out of a model
// that ended its previous turn silent after a tool-call round.
//
// Streams the response via ChatStream when emitChunk is non-nil so the
// website's assistant bubble fills live, exactly like the primary turn.
// Without streaming the user used to see nothing during rescue (chunks
// only arrive from streaming providers) and had to reload the page to
// see the saved final text — a clearly broken UX. Falls back to a plain
// Chat call when no emitter is wired (RunSync paths, tests).
//
// Scope kept narrow: single call, tools=nil. If the model tries to call
// a tool it's rejected at the provider edge. If it still returns empty,
// the caller falls through to the localised fallback sentence.
//
// Future work: a full retry-with-tools needs to re-enter the pipeline
// stages (BuildFilteredTools → CallLLM → HandleToolCalls) and is blocked
// on refactoring FinalizeStage to expose the provider/state plumbing.
func (l *Loop) rescueEmptyReply(
	ctx context.Context,
	history []providers.Message,
	emitChunk func(content string),
) string {
	if l.provider == nil || l.model == "" || len(history) == 0 {
		return ""
	}

	nudge := providers.Message{
		Role: "user",
		Content: "[System] You ended your previous turn with an empty reply. " +
			"Using the information you've already gathered above (tool results, " +
			"prior messages), give the user a direct answer now. Do not call " +
			"any more tools — reply in plain text only.",
	}

	msgs := make([]providers.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, nudge)

	req := providers.ChatRequest{
		Messages: msgs,
		Tools:    nil, // force text
		Model:    l.model,
		Options: map[string]any{
			providers.OptStripThinking: true,
		},
	}

	var resp *providers.ChatResponse
	var err error
	if emitChunk != nil {
		resp, err = l.provider.ChatStream(ctx, req, func(chunk providers.StreamChunk) {
			if chunk.Content != "" {
				emitChunk(chunk.Content)
			}
		})
	} else {
		resp, err = l.provider.Chat(ctx, req)
	}
	if err != nil {
		slog.Warn("agent: rescue retry failed", "error", err)
		return ""
	}
	if resp == nil {
		return ""
	}
	return strings.TrimSpace(resp.Content)
}

// makeRescueChunkEmitter builds a per-token chunk emitter targeting the
// current run's WS subscription, so rescueEmptyReply's streamed content
// reaches the same `chunk` event handler the primary turn used. Returns
// nil if the run can't be addressed (no tenant, no emit machinery).
//
// We deliberately reuse `protocol.ChatEventChunk` rather than introducing
// a new "rescue.chunk" type — the frontend doesn't need to distinguish
// rescue from primary; both just append to the bubble.
func (l *Loop) makeRescueChunkEmitter(req *RunRequest) func(string) {
	if l == nil || req == nil {
		return nil
	}
	emitRun := makeToolEmitRun(l, req)
	return func(content string) {
		if content == "" {
			return
		}
		emitRun(AgentEvent{
			Type:    "chunk", // protocol.ChatEventChunk
			AgentID: l.id,
			RunID:   req.RunID,
			Payload: map[string]string{"content": content},
		})
	}
}
