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
func (l *Loop) fetchConnectedChannels(ctx context.Context) []ConnectedChannelSummary {
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
		summary := ConnectedChannelSummary{
			Name:        inst.Name,
			ChannelType: inst.ChannelType,
			DisplayName: inst.DisplayName,
		}
		// Surface auto_link_user_id so the agent knows which human owns the
		// bot — useful when deciding whose chat a reminder should land in.
		if len(inst.Config) > 0 {
			var cfg struct {
				AutoLinkUserID string `json:"auto_link_user_id"`
			}
			if err := json.Unmarshal(inst.Config, &cfg); err == nil && cfg.AutoLinkUserID != "" {
				summary.OwnerHint = cfg.AutoLinkUserID
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
		if c.OwnerHint != "" {
			line += fmt.Sprintf(" — owner user_id: `%s`", c.OwnerHint)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nFor the current session itself use `deliver_channel=\"ws\"` ")
	b.WriteString("(or the session's own channel name if the user is on Telegram/Slack/etc.) ")
	b.WriteString("and `deliver_to` = current session key.\n")
	return []string{b.String()}
}
