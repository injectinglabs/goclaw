package methods

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"log/slog"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// ChatMethods handles chat.send, chat.history, chat.abort, chat.inject.
type ChatMethods struct {
	agents      *agent.Router
	sessions    store.SessionStore
	cfg         *config.Config
	rateLimiter *gateway.RateLimiter
	eventBus    bus.EventPublisher
	postTurn    tools.PostTurnProcessor
	audioMgr    *audio.Manager  // for TTS auto-apply on WS responses (nil = disabled)
	tools       *tools.Registry // used to route chat.toolResult → pending client tool channels
}

func NewChatMethods(agents *agent.Router, sess store.SessionStore, cfg *config.Config, rl *gateway.RateLimiter, eventBus bus.EventPublisher) *ChatMethods {
	return &ChatMethods{agents: agents, sessions: sess, cfg: cfg, rateLimiter: rl, eventBus: eventBus}
}

// SetAudioManager sets the audio manager for TTS auto-apply on WS responses.
func (m *ChatMethods) SetAudioManager(mgr *audio.Manager) {
	m.audioMgr = mgr
}

// SetToolRegistry sets the tool registry so handleToolResult can route client-tool
// results back to the agent goroutine that is blocked waiting on the call.
func (m *ChatMethods) SetToolRegistry(reg *tools.Registry) {
	m.tools = reg
}

// SetPostTurnProcessor sets the post-turn processor for team task dispatch.
func (m *ChatMethods) SetPostTurnProcessor(pt tools.PostTurnProcessor) {
	m.postTurn = pt
}

// Register adds chat methods to the router.
func (m *ChatMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodChatSend, m.handleSend)
	router.Register(protocol.MethodChatHistory, m.handleHistory)
	router.Register(protocol.MethodChatAbort, m.handleAbort)
	router.Register(protocol.MethodChatInject, m.handleInject)
	router.Register(protocol.MethodChatSessionStatus, m.handleSessionStatus)
	router.Register(protocol.MethodChatToolResult, m.handleToolResult)
}

// handleSessionStatus returns the running state and activity for a session.
// Used by the frontend to restore UI state after switching between sessions.
func (m *ChatMethods) handleSessionStatus(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params struct {
		SessionKey string `json:"sessionKey"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.SessionKey == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "sessionKey")))
		return
	}

	// Ownership check: non-admin users can only query their own sessions.
	if !requireSessionOwner(ctx, m.sessions, m.cfg, client, req.ID, params.SessionKey) {
		return
	}

	isRunning := m.agents.IsSessionBusy(params.SessionKey)
	var runId string
	if rid, ok := m.agents.SessionRunID(params.SessionKey); ok {
		runId = rid
	}
	var activity map[string]any
	if status := m.agents.GetActivity(params.SessionKey); status != nil {
		activity = map[string]any{
			"phase":     status.Phase,
			"tool":      status.Tool,
			"iteration": status.Iteration,
		}
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"isRunning": isRunning,
		"runId":     runId,
		"activity":  activity,
	}))
}

// chatMediaItem represents a media file attached to a chat message.
type chatMediaItem struct {
	Path     string `json:"path"`
	Filename string `json:"filename,omitempty"`
}

type chatSendParams struct {
	Message    string          `json:"message"`
	AgentID    string          `json:"agentId"`
	SessionKey string          `json:"sessionKey"`
	Stream     bool            `json:"stream"`
	Media      json.RawMessage `json:"media,omitempty"` // []string (legacy) or []chatMediaItem
	PageHint   *pageHint       `json:"pageHint,omitempty"`
	Model      string          `json:"model,omitempty"`
	// ClientKind identifies the WS caller so the tool palette can be tailored:
	// "extension" exposes browser page-tools (execute_action, execute_js, etc.),
	// any other value (e.g. "website") strips them. Empty = legacy/extension
	// behavior — backward compatible until all clients tag themselves.
	ClientKind string `json:"clientKind,omitempty"`
}

// pageHint carries the URL+title of the user's currently active browser tab.
// Sent by the web-agent extension on every chat.send so the LLM can decide
// whether to call refresh_page_content without paying for an unconditional
// HTML snapshot per turn.
type pageHint struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// parseMedia handles both legacy string paths and new {path,filename} objects.
func (p *chatSendParams) parseMedia() []chatMediaItem {
	if len(p.Media) == 0 {
		return nil
	}
	// Try new format: [{path, filename}]
	var items []chatMediaItem
	if err := json.Unmarshal(p.Media, &items); err == nil {
		return items
	}
	// Fallback: legacy ["path1", "path2"]
	var paths []string
	if err := json.Unmarshal(p.Media, &paths); err == nil {
		for _, path := range paths {
			items = append(items, chatMediaItem{Path: path})
		}
		return items
	}
	return nil
}

func (m *ChatMethods) handleSend(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	// Rate limit check per user/client
	if m.rateLimiter != nil && m.rateLimiter.Enabled() {
		key := client.UserID()
		if key == "" {
			key = client.ID()
		}
		if !m.rateLimiter.Allow(key) {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRateLimitExceeded)))
			return
		}
	}

	var params chatSendParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgInvalidJSON)))
		return
	}

	if params.AgentID == "" {
		// Extract agent key from session key (format: "agent:{key}:{rest}")
		// so resuming an existing session routes to the correct agent.
		if params.SessionKey != "" {
			if agentKey, _ := sessions.ParseSessionKey(params.SessionKey); agentKey != "" {
				params.AgentID = agentKey
			}
		}
		if params.AgentID == "" {
			params.AgentID = "default"
		}
	}

	loop, err := m.agents.Get(ctx, params.AgentID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, err.Error()))
		return
	}

	userID := client.UserID()
	if userID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgUserIDRequired)))
		return
	}

	runID := uuid.NewString()
	sessionKey := params.SessionKey
	if sessionKey == "" {
		sessionKey = sessions.BuildWSSessionKey(params.AgentID, uuid.NewString())
	}

	// Ownership check: when resuming an existing session, verify the caller owns it.
	// Skip for new sessions (Get returns nil) so first-message creation is not blocked.
	if params.SessionKey != "" && !canSeeAll(client.Role(), m.cfg.Gateway.OwnerIDs, userID) {
		if sess := m.sessions.Get(ctx, sessionKey); sess != nil && sess.UserID != userID {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrUnauthorized, i18n.T(locale, i18n.MsgPermissionDenied, "session")))
			return
		}
	}

	// Detach from HTTP request context so agent runs survive page navigation/reconnect.
	// WithoutCancel preserves all context values (locale, user ID, etc.)
	// but HTTP request cancellation no longer propagates.
	// Explicit abort via chat.abort still works through the per-run cancel().
	runCtxBase := context.WithoutCancel(ctx)
	if userID != "" {
		runCtxBase = store.WithUserID(runCtxBase, userID)
	}

	// Mid-run injection: if session already has an active run, inject the message
	// into the running loop instead of starting a new concurrent run.
	if m.agents.IsSessionBusy(sessionKey) {
		// Exact cancel keyword detection: auto-abort when user sends "stop", "cancel", etc.
		if agent.IsExactCancelKeyword(params.Message) {
			results := m.agents.AbortRunsForSession(sessionKey)
			aborted := false
			for _, r := range results {
				if r.Stopped || r.Forced {
					aborted = true
					break
				}
			}
			client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
				"cancelled": true,
				"aborted":   aborted,
			}))
			return
		}

		injected := m.agents.InjectMessage(sessionKey, agent.InjectedMessage{
			Content: params.Message,
			UserID:  userID,
		})
		if injected {
			client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
				"injected": true,
			}))
			return
		}
		// Fallback: injection failed (channel full), proceed with new run
	}

	// Inject team dispatch tracker: gates team_tasks create (must search/list first)
	// and defers task dispatch to post-turn.
	runCtxBase, drainTeamDispatch := tools.InjectTeamDispatch(runCtxBase, m.postTurn)

	// Create cancellable context for abort support (matching TS AbortController pattern).
	runCtx, cancel := context.WithCancel(runCtxBase)
	injectCh := m.agents.RegisterRun(runCtxBase, runID, sessionKey, params.AgentID, cancel)

	// Run agent asynchronously - events are broadcast via the event system
	go func() {
		defer m.agents.UnregisterRun(runID)
		defer cancel()
		defer drainTeamDispatch() // dispatch pending team tasks + release lock (even on panic)

		// Parse media items (supports both legacy string paths and new {path,filename} objects).
		items := params.parseMedia()

		// Convert media items to bus.MediaFile with MIME detection.
		var mediaFiles []bus.MediaFile
		var mediaInfos []media.MediaInfo
		for _, item := range items {
			mimeType := media.DetectMIMEType(item.Path)
			mediaFiles = append(mediaFiles, bus.MediaFile{Path: item.Path, MimeType: mimeType, Filename: item.Filename})
			mediaInfos = append(mediaInfos, media.MediaInfo{
				Type:        media.MediaKindFromMime(mimeType),
				FilePath:    item.Path,
				ContentType: mimeType,
				FileName:    item.Filename,
			})
		}

		// Prepend media tags so the LLM knows what media is attached.
		message := params.Message
		if len(mediaInfos) > 0 {
			if tags := media.BuildMediaTags(mediaInfos); tags != "" {
				if message != "" {
					message = tags + "\n\n" + message
				} else {
					message = tags
				}
			}
		}

		// Prepend page_hint so the LLM sees the user's current browser tab URL+title
		// on every turn. Lets the model decide whether to call refresh_page_content
		// instead of paying for an unconditional HTML snapshot per message.
		if params.PageHint != nil && params.PageHint.URL != "" {
			hint := "[current page: " + params.PageHint.URL
			if params.PageHint.Title != "" {
				hint += " — " + params.PageHint.Title
			}
			hint += "]"
			if message != "" {
				message = hint + "\n\n" + message
			} else {
				message = hint
			}
		}

		result, err := loop.Run(runCtx, agent.RunRequest{
			SessionKey:    sessionKey,
			Message:       message,
			Media:         mediaFiles,
			Channel:       "ws",
			ChatID:        userID, // use stable userID for team/workspace isolation (not ephemeral client.ID())
			RunID:         runID,
			UserID:        userID,
			Stream:        params.Stream,
			InjectCh:      injectCh,
			ModelOverride: params.Model,
			ClientKind:    params.ClientKind,
			// Wire trace ID back to the active run so force-abort can mark the
			// correct trace as cancelled if the goroutine does not exit within 3s.
			OnTraceCreated: func(traceID uuid.UUID) {
				m.agents.SetRunTraceID(runID, traceID)
			},
		})

		if err != nil {
			// Send cancelled response so the frontend's chat.send promise resolves
			// instead of hanging until the 600s timeout.
			if runCtx.Err() != nil {
				client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
					"cancelled": true,
				}))
				return
			}
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
			return
		}

		// Auto-generate conversation title on first message (label empty = never titled).
		if label := m.sessions.GetLabel(ctx, sessionKey); label == "" {
			agentProvider := loop.Provider()
			agentModel := loop.Model()
			userMsg := params.Message
			// Use runCtxBase (WithoutCancel + tenant-aware) so title save uses correct tenant.
			titleCtx := runCtxBase
			// Attach actor headers for the outbound /v1/chat/completions
			// call so the downstream service-token endpoint can attribute
			// this background turn to the right user/org. Without this,
			// GenerateTitle bypassed Loop.injectContext (which is where the
			// regular run path sets these), and the outbound arrived with
			// only Authorization=service-token and no X-Actor-* headers —
			// web-agent-api 400'd with "requires X-Actor-User-ID and
			// X-Actor-Org-ID" on every new-chat first message.
			if userID != "" {
				// Prefer the web-backend org UUID stamped onto the
				// tenant by auth-proxy (see resolveTenantSlugAndExternalOrgID
				// + tenants.settings.external_org_id) — keeps title-gen
				// in sync with the regular run path in injectContext.
				// Falls back to the goclaw slug for tenants the
				// auth-proxy hasn't stamped yet during rollout.
				orgID := loop.ExternalOrgID()
				if orgID == "" {
					orgID = store.TenantSlugFromContext(runCtxBase)
				}
				if orgID == "" {
					orgID = loop.TenantSlug()
				}
				if orgID != "" {
					titleCtx = providers.WithActorHeaders(titleCtx, map[string]string{
						"X-Actor-User-ID": userID,
						"X-Actor-Org-ID":  orgID,
					})
				}
			}
			go func() {
				title := agent.GenerateTitle(titleCtx, agentProvider, agentModel, userMsg)
				if title == "" {
					return
				}
				m.sessions.SetLabel(titleCtx, sessionKey, title)
				if err := m.sessions.Save(titleCtx, sessionKey); err != nil {
					slog.Warn("failed to save session title", "sessionKey", sessionKey, "error", err)
					return
				}
				bus.BroadcastForTenant(m.eventBus, protocol.EventSessionUpdated,
					client.TenantID(),
					map[string]string{"sessionKey": sessionKey, "label": title, "userId": userID})
			}()
		}

		// TTS auto-apply: convert [[tts]] tagged responses to voice audio
		content := result.Content
		var ttsAudio *agent.MediaResult
		if m.audioMgr != nil && content != "" {
			// For WS, we don't have voice inbound info - use "tagged" mode only
			ttsResult, _ := m.audioMgr.AutoApplyToText(runCtx, content, "ws", false, "")
			if ttsResult != nil && ttsResult.AudioPath != "" {
				// Include audio in media results
				ttsAudio = &agent.MediaResult{
					Path:        httpapi.SignMediaPath(ttsResult.AudioPath, httpapi.FileSigningKey()),
					ContentType: ttsResult.AudioMime,
					AsVoice:     true,
				}
				content = ttsResult.Text // Use stripped text
			} else if ttsResult != nil {
				content = ttsResult.Text // Strip directives even if TTS not applied
			}
		}

		resp := map[string]any{
			"runId":   result.RunID,
			"content": content,
			"usage":   result.Usage,
		}
		if result.Thinking != "" {
			resp["thinking"] = result.Thinking
		}
		// Combine existing media with TTS audio
		mediaResults := result.Media
		if ttsAudio != nil {
			mediaResults = append([]agent.MediaResult{*ttsAudio}, mediaResults...)
		}
		if len(mediaResults) > 0 {
			resp["media"] = mediaResults
		}
		client.SendResponse(protocol.NewOKResponse(req.ID, resp))
	}()
}

type chatHistoryParams struct {
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey"`
}

func (m *ChatMethods) handleHistory(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params chatHistoryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgInvalidJSON)))
		return
	}

	if params.AgentID == "" {
		params.AgentID = "default"
	}

	sessionKey := params.SessionKey
	if sessionKey == "" {
		sessionKey = sessions.BuildWSSessionKey(params.AgentID, uuid.NewString())
	}

	// Ownership check: non-admin users can only read their own session history.
	if params.SessionKey != "" && !requireSessionOwner(ctx, m.sessions, m.cfg, client, req.ID, sessionKey) {
		return
	}

	history := m.sessions.GetHistory(ctx, sessionKey)

	// Sign file URLs before delivery — sessions store clean paths.
	secret := httpapi.FileSigningKey()
	for i := range history {
		history[i].Content = httpapi.SignFileURLs(history[i].Content, secret)
		for j := range history[i].MediaRefs {
			history[i].MediaRefs[j].Path = httpapi.SignMediaPath(history[i].MediaRefs[j].Path, secret)
		}
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"messages": history,
	}))
}

// handleInject injects a message into a session transcript without running the agent.
// Matching TS chat.inject (src/gateway/server-methods/chat.ts:686-746).
func (m *ChatMethods) handleInject(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params struct {
		SessionKey string `json:"sessionKey"`
		Message    string `json:"message"`
		Label      string `json:"label"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgInvalidJSON)))
		return
	}

	if params.SessionKey == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "sessionKey")))
		return
	}
	if params.Message == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgMsgRequired)))
		return
	}

	// Ownership check: non-admin users can only inject into their own sessions.
	if !requireSessionOwner(ctx, m.sessions, m.cfg, client, req.ID, params.SessionKey) {
		return
	}

	// Truncate label
	if len(params.Label) > 100 {
		params.Label = params.Label[:100]
	}

	// Build content text
	text := params.Message
	if params.Label != "" {
		text = "[" + params.Label + "]\n\n" + params.Message
	}

	// Create an assistant message with gateway-injected metadata
	messageID := uuid.NewString()
	m.sessions.AddMessage(ctx, params.SessionKey, providers.Message{
		Role:    "assistant",
		Content: text,
	})

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"ok":        true,
		"messageId": messageID,
	}))
}

// handleAbort cancels running agent invocations.
// Matching TS chat-abort.ts: validates sessionKey, supports per-runId or per-session abort.
//
// Params:
//
//	{ sessionKey: string, runId?: string }
//
// Response:
//
//	{ ok: true, aborted: bool, stopped: bool, forced: bool,
//	  alreadyAborting: bool, notFound: bool, unauthorized: bool, runIds: []string }
func (m *ChatMethods) handleAbort(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params struct {
		RunID      string `json:"runId"`
		SessionKey string `json:"sessionKey"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgInvalidJSON)))
		return
	}

	if params.SessionKey == "" && params.RunID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "sessionKey or runId")))
		return
	}

	// Non-admin users must provide sessionKey for ownership verification.
	if params.SessionKey == "" && params.RunID != "" && !canSeeAll(client.Role(), m.cfg.Gateway.OwnerIDs, client.UserID()) {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "sessionKey")))
		return
	}

	// Ownership check: non-admin users can only abort their own sessions.
	if params.SessionKey != "" && !requireSessionOwner(ctx, m.sessions, m.cfg, client, req.ID, params.SessionKey) {
		return
	}

	isAdmin := canSeeAll(client.Role(), m.cfg.Gateway.OwnerIDs, client.UserID())

	// Collect abort results.
	var results []agent.AbortResult
	if params.RunID != "" {
		results = []agent.AbortResult{m.agents.AbortRun(params.RunID, params.SessionKey)}
	} else {
		results = m.agents.AbortRunsForSession(params.SessionKey)
	}

	// Aggregate counts and run IDs.
	var runIDs []string
	stopped, forced, alreadyAborting, notFound, unauthorized := 0, 0, 0, 0, 0
	for _, r := range results {
		runIDs = append(runIDs, r.RunID)
		switch {
		case r.Stopped:
			stopped++
		case r.Forced:
			forced++
		case r.AlreadyAborting:
			alreadyAborting++
		case r.NotFound:
			notFound++
		case r.Unauthorized:
			unauthorized++
			slog.Warn("chat.abort: unauthorized run abort attempt",
				"runId", r.RunID, "userID", client.UserID())
		}
	}

	// Security: collapse Unauthorized → NotFound for non-admin callers so run
	// existence is not leaked to unprivileged clients.
	respUnauthorized := unauthorized
	if !isAdmin && unauthorized > 0 {
		notFound += unauthorized
		respUnauthorized = 0
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"ok":              true,
		"aborted":         stopped+forced > 0,
		"stopped":         stopped > 0,
		"forced":          forced > 0,
		"alreadyAborting": alreadyAborting > 0,
		"notFound":        notFound > 0 && stopped+forced+alreadyAborting == 0,
		"unauthorized":    respUnauthorized > 0,
		"runIds":          runIDs,
	}))
}

// chatToolResultParams is the payload the browser extension sends after executing
// a client tool (refresh_page_content, execute_action). The goclaw agent loop is
// blocked on a channel keyed by toolCallId; we route `content` into that channel
// and let the loop continue.
type chatToolResultParams struct {
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"`
	IsError    bool   `json:"isError"`
}

// handleToolResult accepts a client-tool response from the extension and routes
// it into the matching pending tool-call channel on the registry. Returns
// ok=true when the waiting goroutine was still there, ok=false when the call
// had already timed out or been cleaned up.
//
// There is no ownership check by sessionKey here: tool_call_ids are UUIDs minted
// per LLM invocation and are not guessable. Misrouting is impossible because the
// channel map is scoped to this process and the ID must be an exact match.
func (m *ChatMethods) handleToolResult(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params chatToolResultParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgInvalidJSON)))
		return
	}
	if params.ToolCallID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "toolCallId")))
		return
	}
	if m.tools == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "tool registry not wired"))
		return
	}

	var result *tools.Result
	if params.IsError {
		result = tools.ErrorResult(params.Content)
	} else {
		result = tools.NewResult(params.Content)
	}

	routed := m.tools.RouteClientToolResult(params.ToolCallID, result)
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"ok":      routed,
		"stale":   !routed,
	}))
}
