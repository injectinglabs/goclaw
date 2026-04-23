package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// makeCronJobHandler creates a cron job handler that routes through the scheduler's cron lane.
// This ensures per-session concurrency control (same job can't run concurrently)
// and integration with /stop, /stopall commands.
// cronHeartbeatWakeFn holds the heartbeat wake function, set after ticker creation.
// Safe because cron jobs only fire after Start(), well after this is set.
var cronHeartbeatWakeFn func(agentID string)

func makeCronJobHandler(sched *scheduler.Scheduler, msgBus *bus.MessageBus, cfg *config.Config, channelMgr *channels.Manager, sessionMgr store.SessionStore, agentStore store.AgentStore, reminderStore store.ReminderStore) func(job *store.CronJob) (*store.CronJobResult, error) {
	return func(job *store.CronJob) (*store.CronJobResult, error) {
		agentID := job.AgentID
		if agentID == "" && agentStore != nil {
			// Resolve real default agent from DB instead of using literal "default" string.
			tenantCtx := store.WithTenantID(context.Background(), job.TenantID)
			if defaultAgent, err := agentStore.GetDefault(tenantCtx); err == nil {
				agentID = defaultAgent.AgentKey
			} else {
				agentID = cfg.ResolveDefaultAgentID()
			}
		} else if agentID == "" {
			agentID = cfg.ResolveDefaultAgentID()
		} else if id, err := uuid.Parse(agentID); err == nil && agentStore != nil {
			// Resolve agentKey from UUID so session key uses agentKey
			// (consistent with chat/WS/team paths, fixes cache invalidation mismatch).
			cronCtx := store.WithTenantID(context.Background(), job.TenantID)
			if ag, err := agentStore.GetByID(cronCtx, id); err == nil {
				agentID = ag.AgentKey
			}
		} else {
			agentID = config.NormalizeAgentID(agentID)
		}

		sessionKey := sessions.BuildCronSessionKey(agentID, job.ID)
		channel := job.DeliverChannel
		if channel == "" {
			channel = "cron"
		}

		// Infer peer kind from the stored session metadata (group chats need it
		// so that tools like message can route correctly via group APIs).
		peerKind := resolveCronPeerKind(job)

		// Resolve channel type for system prompt context.
		channelType := resolveChannelType(channelMgr, channel)

		// Build cron context so the agent knows delivery target and requester.
		var extraPrompt string
		if job.Deliver && job.DeliverChannel != "" && job.DeliverTo != "" {
			extraPrompt = fmt.Sprintf(
				"[Cron Job]\nThis is scheduled job \"%s\" (ID: %s).\n"+
					"Requester: user %s on channel \"%s\" (chat %s).\n"+
					"Your response will be automatically delivered to that chat — just produce the content directly.",
				job.Name, job.ID, job.UserID, job.DeliverChannel, job.DeliverTo,
			)
		} else {
			extraPrompt = fmt.Sprintf(
				"[Cron Job]\nThis is scheduled job \"%s\" (ID: %s), created by user %s.\n"+
					"Delivery is not configured — respond normally.",
				job.Name, job.ID, job.UserID,
			)
		}

		// Build context with tenant scope and timeout so agent loop events are
		// scoped correctly and a hung agent can't block the cron scheduler forever.
		jobTimeout := cfg.Cron.JobTimeoutDuration()
		cronCtx, cancelCron := context.WithTimeout(context.Background(), jobTimeout)
		defer cancelCron()
		cronCtx = store.WithTenantID(cronCtx, job.TenantID)

		// Reset session before each cron run to prevent tool errors from previous
		// runs from polluting the context and blocking future executions (#294).
		// Save() persists the empty session to DB so stale data won't reload after restart.
		// Stateless jobs skip this — they intentionally carry no session history.
		if !job.Stateless {
			sessionMgr.Reset(cronCtx, sessionKey)
			sessionMgr.Save(cronCtx, sessionKey)
		}

		// Schedule through cron lane — scheduler handles agent resolution and concurrency
		outCh := sched.Schedule(cronCtx, scheduler.LaneCron, agent.RunRequest{
			SessionKey:        sessionKey,
			Message:           job.Payload.Message,
			Channel:           channel,
			ChannelType:       channelType,
			ChatID:            job.DeliverTo,
			PeerKind:          peerKind,
			UserID:            job.UserID,
			RunID:             fmt.Sprintf("cron:%s", job.ID),
			Stream:            false,
			ExtraSystemPrompt: extraPrompt,
			TraceName:         fmt.Sprintf("Cron [%s] - %s", job.Name, agentID),
			TraceTags:         []string{"cron"},
		})

		// Block until the scheduled run completes or the timeout fires.
		var outcome scheduler.RunOutcome
		select {
		case outcome = <-outCh:
		case <-cronCtx.Done():
			return nil, fmt.Errorf("cron job %s timed out after %s", job.Name, jobTimeout)
		}
		if outcome.Err != nil {
			return nil, outcome.Err
		}

		result := outcome.Result

		// If job wants delivery to a channel, send the agent response to the target chat.
		if job.Deliver && job.DeliverChannel != "" && job.DeliverTo != "" {
			// Internal channels (browser, ws, cli, system, subagent) never reach the
			// outbound dispatcher — docs/05-channels-messaging.md states they "use
			// WebSocket directly on the gateway connection". Broadcast an event on
			// the gateway bus so any connected WS client scoped to this tenant can
			// inject the cron result straight into its session. Keeps cron delivery
			// working for the extension / CLI without bending the outbound path.
			if channels.IsInternalChannel(job.DeliverChannel) {
				slog.Info("cron: delivering to internal channel",
					"job_id", job.ID, "channel", job.DeliverChannel, "session_key", job.DeliverTo, "content_len", len(result.Content))
				trimmed := strings.TrimSpace(result.Content)
				// Reminders live in their own table — NOT in sessions.messages.
				// That keeps `sessions.messages` pure conversation (user+agent
				// turns), so next-turn agent context doesn't carry forward the
				// reminder text as if it were user-visible dialog. UI on the
				// client side merges sessions.messages and reminders (scoped
				// by origin_session_key) chronologically for display.
				if reminderStore != nil && trimmed != "" {
					rem := &store.Reminder{
						TenantID:         job.TenantID,
						UserID:           job.UserID,
						JobID:            job.ID,
						JobName:          job.Name,
						OriginSessionKey: job.DeliverTo,
						Channel:          job.DeliverChannel,
						Content:          result.Content,
					}
					if err := reminderStore.Insert(cronCtx, rem); err != nil {
						slog.Warn("cron: reminder persist failed",
							"job_id", job.ID, "error", err)
					}
				}
				// 3. Live broadcast — gives connected WS clients the message
				//    without a chat.preview roundtrip.
				bus.BroadcastForTenant(msgBus, protocol.EventCronDelivered, job.TenantID,
					map[string]any{
						"session_key":      job.DeliverTo,
						"cron_session_key": sessionKey,
						"channel":          job.DeliverChannel,
						"content":          result.Content,
						"media":            result.Media,
						"agent_id":         agentID,
						"job_id":           job.ID,
						"job_name":         job.Name,
					})
			} else {
				outMsg := bus.OutboundMessage{
					Channel: job.DeliverChannel,
					ChatID:  job.DeliverTo,
					Content: result.Content,
				}
				if peerKind == "group" {
					outMsg.Metadata = map[string]string{"group_id": job.DeliverTo}
				}
				appendMediaToOutbound(&outMsg, result.Media)
				msgBus.PublishOutbound(outMsg)
			}
		} else if job.Deliver {
			slog.Warn("cron: delivery configured but channel/chatID missing — output discarded",
				"job_id", job.ID, "job_name", job.Name, "channel", job.DeliverChannel, "to", job.DeliverTo)
		}

		cronResult := &store.CronJobResult{
			Content: result.Content,
		}
		if result.Usage != nil {
			cronResult.InputTokens = result.Usage.PromptTokens
			cronResult.OutputTokens = result.Usage.CompletionTokens
		}

		// wakeMode: trigger heartbeat after cron job completes.
		// Use original job.AgentID (UUID) — cronHeartbeatWakeFn expects UUID for ticker.Wake().
		if job.WakeHeartbeat && cronHeartbeatWakeFn != nil {
			cronHeartbeatWakeFn(job.AgentID)
		}

		return cronResult, nil
	}
}

// resolveCronPeerKind infers peer kind from the cron job's user ID.
// Group cron jobs have userID prefixed with "group:" or "guild:" (set during job creation).
func resolveCronPeerKind(job *store.CronJob) string {
	if strings.HasPrefix(job.UserID, "group:") || strings.HasPrefix(job.UserID, "guild:") {
		return "group"
	}
	return ""
}
