package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// fetchConnectedChannels returns a lightweight snapshot of the tenant's
// currently-enabled channel_instances for rendering into the system prompt.
// Failure is non-fatal: on any error we log at DEBUG and return nil so the
// prompt simply omits the section. Only enabled, non-internal instances are
// included.
//
// callerUserID scopes the snapshot to channels the caller themselves
// connected — for shared (multi-member) tenants we must not surface another
// member's bot in the system prompt, otherwise the model will reference it
// regardless of what list_connected_channels returns. Default-seeded
// instances (CreatedBy == "") stay visible to everyone.
func (l *Loop) fetchConnectedChannels(ctx context.Context, callerUserID string) []ConnectedChannelSummary {
	if l.channelInstanceStore == nil {
		return nil
	}
	instances, err := l.channelInstanceStore.ListAllEnabled(ctx)
	if err != nil {
		slog.Debug("connected_channels.list_failed", "agent", l.id, "error", err)
		return nil
	}
	out := make([]ConnectedChannelSummary, 0, len(instances))
	for _, inst := range instances {
		if inst.AgentID != l.agentUUID {
			continue // belongs to a different agent in the same tenant
		}
		// Per-user isolation in shared tenants: skip instances connected by
		// someone else. A blank CreatedBy means the row was default-seeded
		// (no creator) and is global — keep showing it. A blank callerUserID
		// means we lack identity context (system / batch path) — fall back
		// to the legacy "show everything for this agent" behaviour rather
		// than hiding everything.
		if callerUserID != "" && inst.CreatedBy != "" && inst.CreatedBy != callerUserID {
			continue
		}
		summary := ConnectedChannelSummary{
			Name:        inst.Name,
			ChannelType: inst.ChannelType,
			DisplayName: inst.DisplayName,
		}
		// Surface auto_link_user_id so the agent knows which human owns the
		// bot — useful when deciding whose chat a reminder should land in.
		if len(inst.Config) > 0 {
			var cfg struct {
				AutoLinkUserID string   `json:"auto_link_user_id"`
				AllowFrom      []string `json:"allow_from"`
				DMPolicy       string   `json:"dm_policy"`
			}
			if err := json.Unmarshal(inst.Config, &cfg); err == nil {
				if cfg.AutoLinkUserID != "" {
					summary.OwnerHint = cfg.AutoLinkUserID
				}
				// dm_policy=allowlist with a single allow_from entry means this bot
				// is dedicated to one peer — that peer's Telegram/Slack/... numeric
				// chat_id is safe to surface as a deliver_to hint so the agent
				// doesn't have to look it up or re-ask the user.
				if cfg.DMPolicy == "allowlist" && len(cfg.AllowFrom) == 1 {
					summary.DeliverTo = strings.TrimPrefix(cfg.AllowFrom[0], "@")
				}
			}
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// buildConnectedChannelsSection renders the snapshot as a terse routing
// reference the model can consult when scheduling a proactive delivery.
// Returns empty when the snapshot is empty so the caller can skip the block.
func buildConnectedChannelsSection(channels []ConnectedChannelSummary) []string {
	if len(channels) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("## Connected Channels\n\n")
	b.WriteString("These channels are already wired for this agent. When scheduling ")
	b.WriteString("proactive delivery (cron, message, sessions_send), pass the ")
	b.WriteString("channel name exactly as listed and set `deliver: true`. Do NOT ")
	b.WriteString("ask the user to re-share credentials or reconnect — the ")
	b.WriteString("connection is already live.\n\n")
	for _, c := range channels {
		label := c.DisplayName
		if label == "" {
			label = c.ChannelType
		}
		line := fmt.Sprintf("- `%s` (%s)", c.Name, label)
		if c.DeliverTo != "" {
			line += fmt.Sprintf(" — use `deliver_to: %s` to reach the owner", c.DeliverTo)
		}
		if c.OwnerHint != "" {
			line += fmt.Sprintf(" — internal user_id: `%s`", c.OwnerHint)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nFor the current session itself use `deliver_channel=\"ws\"` ")
	b.WriteString("(or the session's own channel name if the user is on Telegram/Slack/etc.) ")
	b.WriteString("and `deliver_to` = current session key.\n")
	return []string{b.String()}
}
