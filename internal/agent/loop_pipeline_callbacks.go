package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/internal/workspace"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// pipelineCallbacks creates all callback closures that capture *Loop.
// Each callback bridges a pipeline.PipelineDeps function to an existing Loop method.
func (l *Loop) pipelineCallbacks(req *RunRequest, bridgeRS *runState) pipelineCallbackSet {
	// Shared emitRun enriches events with routing context (matching v2 pattern).
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
		event.TenantID = l.tenantID
		l.emit(event)
	}
	// Shared state between enrichMedia (which persists the upload and
	// computes MediaRefs) and flushMessages (which writes the user row).
	// Without this, MediaRefs land nowhere and the frontend's reload
	// path sees `media_refs: []` after a refresh — attachments disappear
	// from the UI even though the bytes were saved to .uploads/.
	var currentMediaRefs []providers.MediaRef
	return pipelineCallbackSet{
		emitRun:            emitRun,
		injectContext:      l.makeInjectContext(req),
		loadSessionHistory: l.makeLoadSessionHistory(),
		resolveWorkspace:   l.makeResolveWorkspace(req),
		loadContextFiles:   l.makeLoadContextFiles(),
		buildMessages:      l.makeBuildMessages(),
		enrichMedia:        l.makeEnrichMedia(req, &currentMediaRefs),
		injectReminders:    l.makeInjectReminders(req),
		buildFilteredTools: l.makeBuildFilteredTools(req),
		callLLM:            l.makeCallLLM(req, emitRun),
		pruneMessages:      l.makePruneMessages(),
		collapseSnapshots:  supersedeStaleSnapshots,
		sanitizeHistory:    sanitizeHistory,
		compactMessages:    l.makeCompactMessages(req),
		runMemoryFlush:     l.makeRunMemoryFlush(),
		executeToolCall:    l.makeExecuteToolCall(req, bridgeRS),
		executeToolRaw:     l.makeExecuteToolRaw(req),
		processToolResult:  l.makeProcessToolResult(req, bridgeRS),
		checkReadOnly:      l.makeCheckReadOnly(req, bridgeRS),
		sanitizeContent:    SanitizeAssistantContent,
		flushMessages:      l.makeFlushMessages(req, &currentMediaRefs),
		updateMetadata:     l.makeUpdateMetadata(req),
		bootstrapCleanup:   l.makeBootstrapCleanup(),
		maybeSummarize:     l.maybeSummarize,
	}
}

// pipelineCallbackSet groups all typed callbacks for PipelineDeps.
type pipelineCallbackSet struct {
	emitRun            func(AgentEvent)
	injectContext      func(ctx context.Context, input *pipeline.RunInput) (context.Context, error)
	loadSessionHistory func(ctx context.Context, sessionKey string) ([]providers.Message, string)
	resolveWorkspace   func(ctx context.Context, input *pipeline.RunInput) (*workspace.WorkspaceContext, error)
	loadContextFiles   func(ctx context.Context, userID string) ([]bootstrap.ContextFile, bool)
	buildMessages      func(ctx context.Context, input *pipeline.RunInput, history []providers.Message, summary string) ([]providers.Message, error)
	enrichMedia        func(ctx context.Context, state *pipeline.RunState) error
	injectReminders    func(ctx context.Context, input *pipeline.RunInput, msgs []providers.Message) []providers.Message
	buildFilteredTools func(state *pipeline.RunState) ([]providers.ToolDefinition, error)
	callLLM            func(ctx context.Context, state *pipeline.RunState, req providers.ChatRequest) (*providers.ChatResponse, error)
	pruneMessages      func(msgs []providers.Message, budget int) ([]providers.Message, pipeline.PruneStats)
	collapseSnapshots  func(msgs []providers.Message) ([]providers.Message, int)
	sanitizeHistory    func(msgs []providers.Message) ([]providers.Message, int)
	compactMessages    func(ctx context.Context, msgs []providers.Message, model string) ([]providers.Message, error)
	runMemoryFlush     func(ctx context.Context, state *pipeline.RunState) error
	executeToolCall    func(ctx context.Context, state *pipeline.RunState, tc providers.ToolCall) ([]providers.Message, error)
	executeToolRaw     func(ctx context.Context, tc providers.ToolCall) (providers.Message, any, error)
	processToolResult  func(ctx context.Context, state *pipeline.RunState, tc providers.ToolCall, rawMsg providers.Message, rawData any) []providers.Message
	checkReadOnly      func(state *pipeline.RunState) (*providers.Message, bool)
	sanitizeContent    func(string) string
	flushMessages      func(ctx context.Context, sessionKey string, msgs []providers.Message) error
	updateMetadata     func(ctx context.Context, sessionKey string, usage providers.Usage, lastPromptTokens, msgCount int) error
	bootstrapCleanup   func(ctx context.Context, state *pipeline.RunState) error
	maybeSummarize     func(ctx context.Context, sessionKey string)
}

func (l *Loop) makeResolveWorkspace(req *RunRequest) func(ctx context.Context, input *pipeline.RunInput) (*workspace.WorkspaceContext, error) {
	resolver := workspace.NewResolver()
	return func(ctx context.Context, input *pipeline.RunInput) (*workspace.WorkspaceContext, error) {
		var teamID *string
		if input.TeamID != "" {
			teamID = &input.TeamID
		}
		return resolver.Resolve(ctx, workspace.ResolveParams{
			AgentID:   l.id,
			AgentType: l.agentType,
			UserID:    input.UserID,
			ChatID:    input.ChatID,
			TenantID:  l.tenantID.String(),
			PeerKind:  input.PeerKind,
			TeamID:    teamID,
			BaseDir:   l.workspace,
		})
	}
}

func (l *Loop) makeLoadContextFiles() func(ctx context.Context, userID string) ([]bootstrap.ContextFile, bool) {
	return func(ctx context.Context, userID string) ([]bootstrap.ContextFile, bool) {
		files := l.resolveContextFiles(ctx, userID)
		hadBootstrap := false
		for _, f := range files {
			if strings.HasSuffix(f.Path, "BOOTSTRAP.md") {
				hadBootstrap = true
				break
			}
		}
		return files, hadBootstrap
	}
}

func (l *Loop) makeBuildMessages() func(ctx context.Context, input *pipeline.RunInput, history []providers.Message, summary string) ([]providers.Message, error) {
	return func(ctx context.Context, input *pipeline.RunInput, history []providers.Message, summary string) ([]providers.Message, error) {
		msgs, _ := l.buildMessages(ctx, history, summary,
			input.Message, input.ExtraSystemPrompt,
			input.SessionKey, input.Channel, input.ChannelType,
			input.ChatTitle, input.PeerKind, input.UserID,
			input.HistoryLimit, input.SkillFilter, input.LightContext)
		return msgs, nil
	}
}

// makeInjectContext wraps injectContext() for the v3 pipeline.
// Reuses the existing injectContext() to avoid logic duplication.
// NOTE: injectContext() and this callback must stay in sync when new context values are added.
func (l *Loop) makeInjectContext(req *RunRequest) func(ctx context.Context, input *pipeline.RunInput) (context.Context, error) {
	return func(ctx context.Context, input *pipeline.RunInput) (context.Context, error) {
		result, err := l.injectContext(ctx, req)
		if err != nil {
			return ctx, err
		}
		// Sync message truncation from req back to pipeline input.
		input.Message = req.Message
		// Cache context window on session (first run only).
		if l.sessions.GetContextWindow(result.ctx, req.SessionKey) <= 0 {
			l.sessions.SetContextWindow(result.ctx, req.SessionKey, l.contextWindow)
		}
		return result.ctx, nil
	}
}

// makeLoadSessionHistory loads session history + summary before BuildMessages.
func (l *Loop) makeLoadSessionHistory() func(ctx context.Context, sessionKey string) ([]providers.Message, string) {
	return func(ctx context.Context, sessionKey string) ([]providers.Message, string) {
		history := l.sessions.GetHistory(ctx, sessionKey)
		summary := l.sessions.GetSummary(ctx, sessionKey)
		return history, summary
	}
}

func (l *Loop) makeEnrichMedia(req *RunRequest, capturedRefs *[]providers.MediaRef) func(ctx context.Context, state *pipeline.RunState) error {
	// mediaRefsAttached gates the SetLastUserMessageMediaRefs call below
	// so it fires exactly once per run — subsequent turns can't dirty
	// the persisted user message.
	var mediaRefsAttached bool
	return func(ctx context.Context, state *pipeline.RunState) error {
		// enrichInputMedia enriches messages in-place: attaches inline images,
		// reloads historical media, enriches <media:*> tags, populates context
		// with refs for tool access. Must receive actual messages (not nil) to
		// avoid index-out-of-range panic on inline image attachment.
		msgs := state.Messages.All()
		if len(msgs) == 0 {
			return nil
		}
		enrichedCtx, enrichedMsgs, mediaRefs := l.enrichInputMedia(ctx, req, msgs)
		if capturedRefs != nil {
			*capturedRefs = mediaRefs
		}
		// Propagate enriched context (media images/docs/audio/video refs for tools).
		state.Ctx = enrichedCtx
		// Update history with enriched messages (media tags, inline images).
		// Skip system message (index 0) — only history + user messages are enriched.
		if len(enrichedMsgs) > 1 {
			state.Messages.SetHistory(enrichedMsgs[1:])
		}

		// Attach the freshly-persisted media refs to the user message
		// chat.go already eager-Save'd at request boundary. Without this
		// step the message is durable but its `media_refs` field is
		// empty, so frontend restores no attachment chips after reload.
		// SetLastUserMessageMediaRefs is idempotent (it overwrites the
		// slice); mediaRefsAttached gates re-runs across turns.
		if !mediaRefsAttached && len(mediaRefs) > 0 {
			if err := l.sessions.SetLastUserMessageMediaRefs(ctx, req.SessionKey, mediaRefs); err != nil {
				slog.Warn("enrichMedia: attach media_refs to user message failed (non-fatal)",
					"sessionKey", req.SessionKey, "error", err)
			} else if err := l.sessions.Save(ctx, req.SessionKey); err != nil {
				slog.Warn("enrichMedia: save after media_refs attach failed (non-fatal)",
					"sessionKey", req.SessionKey, "error", err)
			}
			mediaRefsAttached = true
		}
		return nil
	}
}

func (l *Loop) makeInjectReminders(req *RunRequest) func(ctx context.Context, input *pipeline.RunInput, msgs []providers.Message) []providers.Message {
	return func(ctx context.Context, input *pipeline.RunInput, msgs []providers.Message) []providers.Message {
		updated, _ := l.injectTeamTaskReminders(ctx, req, msgs)
		return updated
	}
}

func (l *Loop) makeBuildFilteredTools(req *RunRequest) func(state *pipeline.RunState) ([]providers.ToolDefinition, error) {
	return func(state *pipeline.RunState) ([]providers.ToolDefinition, error) {
		// Load per-user MCP tools (Notion, etc.) into registry before filtering.
		// Servers with require_user_credentials are deferred at startup and
		// connected per-request here with the actual user's credentials.
		l.getUserMCPTools(state.Ctx, state.Input.UserID)

		maxIter := l.effectiveMaxIterations(req)
		allMsgs := state.Messages.All()
		toolDefs, _, returnedMsgs := l.buildFilteredTools(req, state.Context.HadBootstrap,
			state.Iteration, maxIter, allMsgs)
		// buildFilteredTools returns the full messages slice; only messages appended
		// beyond the original length are injections (e.g. final-iteration hint).
		// Appending the entire slice would duplicate system+history into pending.
		if len(returnedMsgs) > len(allMsgs) {
			for _, msg := range returnedMsgs[len(allMsgs):] {
				state.Messages.AppendPending(msg)
			}
		}
		return toolDefs, nil
	}
}

func (l *Loop) makeCallLLM(req *RunRequest, emitRun func(AgentEvent)) func(ctx context.Context, state *pipeline.RunState, chatReq providers.ChatRequest) (*providers.ChatResponse, error) {
	return func(ctx context.Context, state *pipeline.RunState, chatReq providers.ChatRequest) (*providers.ChatResponse, error) {
		provider := state.Provider
		model := state.Model

		// Enrich ChatRequest options to match v2 (providers need these for caching, routing, audit).
		if chatReq.Options == nil {
			chatReq.Options = make(map[string]any)
		}
		chatReq.Options[providers.OptTemperature] = config.DefaultTemperature
		chatReq.Options[providers.OptSessionKey] = req.SessionKey
		chatReq.Options[providers.OptAgentID] = l.agentUUID.String()
		chatReq.Options[providers.OptUserID] = req.UserID
		// Forward the canonical user id as OpenAI's standard `user` field in
		// the request body (OpenAI-compatible providers accept this for
		// attribution). resolveCredentialUserID picks up merged_id when a
		// channel contact is linked to a tenant_user — gives a stable id
		// across channels that LLM-Service can use as fallback attribution.
		if credUserID := l.resolveCredentialUserID(ctx, *req); credUserID != "" {
			chatReq.Options[providers.OptUser] = credUserID
		}
		chatReq.Options[providers.OptChannel] = req.Channel
		chatReq.Options[providers.OptChatID] = req.ChatID
		chatReq.Options[providers.OptPeerKind] = req.PeerKind
		chatReq.Options[providers.OptLocalKey] = req.LocalKey
		chatReq.Options[providers.OptWorkspace] = tools.ToolWorkspaceFromCtx(ctx)
		if tid := store.TenantIDFromContext(ctx); tid != uuid.Nil {
			chatReq.Options[providers.OptTenantID] = tid.String()
		}

		// Reasoning decision: resolve effort level for thinking models (o3, DeepSeek-R1, Kimi).
		// When an agent has no explicit effort, ParseReasoningConfig defaults to
		// "off" — which sends NO reasoning_effort, so thinking models (notably
		// gemini-3.5-flash, our `default`) run at their MAX thinking budget and
		// spiral: a 100-row sheet burned 1.3–1.8M tokens / 3–4 min reasoning and
		// often emitted no answer (→ markdown fallback). Apply a concrete bounded
		// default so the per-turn turnEffort capping below actually engages.
		// Tunable via GOCLAW_DEFAULT_REASONING_EFFORT; set "off" to restore the
		// old unbounded behaviour. Non-thinking providers ignore it (ResolveReasoning
		// Decision returns off unless the provider is ThinkingCapable).
		reasoningDecision := providers.ResolveReasoningDecision(
			provider, model,
			effectiveReasoningEffort(l.reasoningConfig.Effort),
			l.reasoningConfig.Fallback,
			l.reasoningConfig.Source,
		)
		if effort := reasoningDecision.RequestEffort(); effort != "" {
			// Cap the per-turn thinking budget so it doesn't dominate wall-clock.
			// The planning turn and recovery turns keep real reasoning (capped at
			// "medium"); routine mid-task turns (write a script, run it, observe,
			// deliver the file) drop to "low". A 100-row sheet build was spending
			// ~70s just thinking across its tool turns — most of it wasted on
			// mechanical steps. Browser runs use the same shape (a tighter cap).
			chatReq.Options[providers.OptThinkingLevel] = turnEffort(req, state, effort)
		}
		if reasoningDecision.StripThinking {
			chatReq.Options[providers.OptStripThinking] = true
		}

		// Emit LLM span start for tracing.
		start := time.Now().UTC()
		var opts []spanOption
		if state.Model != "" {
			opts = append(opts, withModel(state.Model))
		}
		if provider != nil {
			opts = append(opts, withProvider(provider.Name()))
		}
		spanID := l.emitLLMSpanStart(ctx, start, state.Iteration+1, chatReq.Messages, opts...)

		var resp *providers.ChatResponse
		var err error
		if req.Stream {
			resp, err = provider.ChatStream(ctx, chatReq, func(chunk providers.StreamChunk) {
				if chunk.Thinking != "" {
					emitRun(AgentEvent{
						Type:    protocol.ChatEventThinking,
						AgentID: l.id,
						RunID:   req.RunID,
						Payload: map[string]string{"content": chunk.Thinking},
					})
				}
				if chunk.Content != "" {
					emitRun(AgentEvent{
						Type:    protocol.ChatEventChunk,
						AgentID: l.id,
						RunID:   req.RunID,
						Payload: map[string]string{"content": chunk.Content},
					})
				}
			})
		} else {
			resp, err = provider.Chat(ctx, chatReq)
		}

		// Non-streaming: emit content events matching v2 behavior (channels need these).
		if !req.Stream && err == nil && resp != nil {
			if resp.Thinking != "" {
				emitRun(AgentEvent{
					Type:    protocol.ChatEventThinking,
					AgentID: l.id,
					RunID:   req.RunID,
					Payload: map[string]string{"content": resp.Thinking},
				})
			}
			if resp.Content != "" {
				emitRun(AgentEvent{
					Type:    protocol.ChatEventChunk,
					AgentID: l.id,
					RunID:   req.RunID,
					Payload: map[string]string{"content": resp.Content},
				})
			}
		}

		l.emitLLMSpanEnd(ctx, spanID, start, resp, err, opts...)
		return resp, err
	}
}

func (l *Loop) makePruneMessages() func(msgs []providers.Message, budget int) ([]providers.Message, pipeline.PruneStats) {
	return func(msgs []providers.Message, budget int) ([]providers.Message, pipeline.PruneStats) {
		var stats pipeline.PruneStats
		pruned := pruneContextMessages(msgs, budget, l.contextPruningCfg, l.tokenCounter, l.model, &stats)
		return pruned, stats
	}
}

// turnEffort caps the per-turn thinking budget for ALL runs. The initial
// planning turn (Iteration 0) and recovery turns (after a tool error or an
// injected loop warning) keep real reasoning but capped at "medium" — 32k-token
// extended thinking is never needed to plan a scrape or fill a web form. Every
// other (routine, mid-task) turn drops to "low": running a script, observing
// its output, and delivering a file don't benefit from deep reasoning, and the
// per-turn thinking was the dominant cost on multi-tool tasks. capEffort only
// ever LOWERS the level, so an agent explicitly configured to "off"/"low" is
// never raised.
// defaultReasoningEffort is the effort applied to thinking-capable models when
// an agent has no explicit reasoning config (ParseReasoningConfig → "off").
// "medium" lets turnEffort keep real reasoning on planning/recovery turns and
// drop routine turns to "low". Override via GOCLAW_DEFAULT_REASONING_EFFORT
// ("off" restores the legacy unbounded behaviour).
var defaultReasoningEffort = func() string {
	if v := strings.TrimSpace(os.Getenv("GOCLAW_DEFAULT_REASONING_EFFORT")); v != "" {
		return v
	}
	return "medium"
}()

// effectiveReasoningEffort applies the bounded default when an agent has no
// explicit effort ("" / "off"), so the per-turn turnEffort cap engages for
// thinking models instead of letting them run unbounded.
func effectiveReasoningEffort(configured string) string {
	if configured == "" || configured == "off" {
		return defaultReasoningEffort
	}
	return configured
}

func turnEffort(_ *RunRequest, state *pipeline.RunState, configured string) string {
	if state.Iteration == 0 || recentTroubleSignal(state) {
		return capEffort(configured, "medium")
	}
	return "low"
}

// capEffort lowers "high" to maxLevel; "low"/"medium"/"off" pass through.
func capEffort(effort, maxLevel string) string {
	if effort == "high" {
		return maxLevel
	}
	return effort
}

// recentTroubleSignal reports whether the last few messages contain a tool
// error or an injected loop warning/critical notice — i.e. the model is
// recovering and should keep full reasoning this turn rather than be downgraded.
func recentTroubleSignal(state *pipeline.RunState) bool {
	msgs := state.Messages.All()
	for i := len(msgs) - 1; i >= 0 && i >= len(msgs)-5; i-- {
		m := msgs[i]
		if m.Role == "tool" && m.IsError {
			return true
		}
		if m.Role == "user" && (strings.Contains(m.Content, "[System: WARNING") || strings.Contains(m.Content, "CRITICAL")) {
			return true
		}
	}
	return false
}

func (l *Loop) makeCompactMessages(req *RunRequest) func(ctx context.Context, msgs []providers.Message, model string) ([]providers.Message, error) {
	return func(ctx context.Context, msgs []providers.Message, model string) ([]providers.Message, error) {
		compacted := l.compactMessagesInPlace(ctx, msgs)
		if compacted == nil {
			return msgs, nil // compaction failed, return original
		}
		// Stamp session metadata with the compaction timestamp so operators
		// can diagnose compaction cadence without a dedicated column. Stored
		// as RFC3339 string in sessions.metadata JSONB (flushed on next save).
		if l.sessions != nil && req != nil && req.SessionKey != "" {
			l.sessions.SetSessionMetadata(ctx, req.SessionKey, map[string]string{
				SessionMetaKeyLastCompactionAt: time.Now().UTC().Format(time.RFC3339),
			})
		}
		return compacted, nil
	}
}

// SessionMetaKeyLastCompactionAt is the sessions.metadata JSONB key used to
// record the RFC3339 timestamp of the most recent compaction. Exported so
// the web UI code path can read it back via GetSessionMetadata without
// duplicating the string.
const SessionMetaKeyLastCompactionAt = "last_compaction_at"

// cacheTouchAt returns the last prune-mutation timestamp for a session.
// Returns zero time if no touch recorded yet.
func (l *Loop) cacheTouchAt(sessionKey string) time.Time {
	if v, ok := l.cacheTouchBySession.Load(sessionKey); ok {
		return v.(time.Time)
	}
	return time.Time{}
}

// markCacheTouched records the current time as the last prune-mutation timestamp
// for the given session. Called only after pruning actually mutates messages.
func (l *Loop) markCacheTouched(sessionKey string) {
	l.cacheTouchBySession.Store(sessionKey, time.Now())
}

func (l *Loop) makeRunMemoryFlush() func(ctx context.Context, state *pipeline.RunState) error {
	return func(ctx context.Context, state *pipeline.RunState) error {
		settings := ResolveMemoryFlushSettings(l.compactionCfg)
		if settings == nil {
			return nil
		}
		l.runMemoryFlush(ctx, state.Input.SessionKey, settings)
		return nil
	}
}

func (l *Loop) makeFlushMessages(req *RunRequest, _ *[]providers.MediaRef) func(ctx context.Context, sessionKey string, msgs []providers.Message) error {
	// User message + media_refs are now eagerly persisted in
	// makeEnrichMedia (before the LLM call) so they're visible to
	// sessions.preview / chat.abort / chat.activeSessions immediately,
	// not only after this end-of-turn flush. flushMessages handles
	// only the pending assistant turn messages now.
	_ = req
	return func(ctx context.Context, sessionKey string, msgs []providers.Message) error {
		for _, msg := range msgs {
			l.sessions.AddMessage(ctx, sessionKey, msg)
		}
		return nil
	}
}

func (l *Loop) makeUpdateMetadata(req *RunRequest) func(ctx context.Context, sessionKey string, usage providers.Usage, lastPromptTokens, msgCount int) error {
	return func(ctx context.Context, sessionKey string, usage providers.Usage, lastPromptTokens, msgCount int) error {
		l.sessions.UpdateMetadata(ctx, sessionKey, l.model, l.provider.Name(), req.Channel)
		l.sessions.AccumulateTokens(ctx, sessionKey, int64(usage.PromptTokens), int64(usage.CompletionTokens))
		// Snapshot of the FINAL iteration's prompt_tokens — what the
		// context-usage indicator reads from sessions.last_prompt_tokens.
		// Skip when zero (provider didn't report usage on this run) so we
		// don't clobber a previously-good value with a meaningless 0.
		if lastPromptTokens > 0 {
			l.sessions.SetLastPromptTokens(ctx, sessionKey, lastPromptTokens, msgCount)
		}
		// Persist session to DB (matching v2 finalizeRun behavior).
		// FlushMessages already ran, so all pending messages are in the cache.
		l.sessions.Save(ctx, sessionKey)
		return nil
	}
}

func (l *Loop) makeSkillPostscript() func(ctx context.Context, content string, totalToolCalls int) string {
	if !l.skillEvolve || l.skillNudgeInterval <= 0 {
		return nil // disabled — FinalizeStage skips
	}
	var sent bool
	return func(ctx context.Context, content string, totalToolCalls int) string {
		if sent || totalToolCalls < l.skillNudgeInterval || IsSilentReply(content) {
			return content
		}
		sent = true
		locale := store.LocaleFromContext(ctx)
		return content + "\n\n---\n_" + i18n.T(locale, i18n.MsgSkillNudgePostscript) + "_"
	}
}

func (l *Loop) makeBootstrapCleanup() func(ctx context.Context, state *pipeline.RunState) error {
	return func(ctx context.Context, state *pipeline.RunState) error {
		if l.bootstrapCleanup == nil {
			return nil
		}
		return l.bootstrapCleanup(ctx, l.agentUUID, state.Input.UserID)
	}
}

