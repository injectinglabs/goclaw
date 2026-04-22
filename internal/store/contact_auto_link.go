package store

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
)

// channelInstanceConfigForAutoLink is a thin view of channel_instances.config
// — only the fields this package needs for auto-linking.
type channelInstanceConfigForAutoLink struct {
	AutoLinkUserID string `json:"auto_link_user_id"`
}

// autoLinkIfOwnerKnown links a freshly-upserted channel_contact to a
// tenant_user identity when the channel_instance declares an owner via
// config.auto_link_user_id. Safe to call redundantly: if the contact is
// already merged, or the channel_instance doesn't declare an owner, or
// stores are nil (collector without channel/tenant lookup wired in), the
// function returns silently. Logs at WARN only for unexpected DB errors —
// a missing instance or a conflict is expected and ignored.
//
// This closes the "write any message to the bot, then ask the agent to
// link my accounts" ritual: by the time the first message hits
// EnsureContact, we already know who the bot's owner is (the channel was
// created with auto_link_user_id by our provisioner), so we can merge
// directly instead of requiring an explicit operator call.
func (c *ContactCollector) autoLinkIfOwnerKnown(ctx context.Context, channelType, channelInstance, senderID string) {
	if c.instances == nil || c.tenants == nil {
		return
	}
	if channelInstance == "" || senderID == "" {
		return
	}

	// Skip if this contact is already merged — avoids a redundant chain of
	// lookups on every repeat inbound message (seen-cache already filters
	// most of them, but this is the authoritative check).
	if resolved, err := c.store.ResolveTenantUserID(ctx, channelType, senderID); err == nil && resolved != "" {
		return
	}

	inst, err := c.instances.GetByName(ctx, channelInstance)
	if err != nil || inst == nil {
		return // unknown instance — nothing to link to
	}
	if len(inst.Config) == 0 {
		return
	}
	var cfg channelInstanceConfigForAutoLink
	if err := json.Unmarshal(inst.Config, &cfg); err != nil || cfg.AutoLinkUserID == "" {
		return
	}

	tenantID := TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return
	}

	// CreateTenantUserReturning is upsert-on-conflict — safe to call repeatedly.
	// Role "owner" matches the semantics of a user who provisioned their own
	// tenant and their own bot. Display name left empty; the bootstrap USER.md
	// template will fill it on first agent turn.
	tu, err := c.tenants.CreateTenantUserReturning(ctx, tenantID, cfg.AutoLinkUserID, "", TenantRoleOwner)
	if err != nil || tu == nil {
		slog.Warn("contact_collector.auto_link.tenant_user_upsert_failed",
			"error", err, "tenant_id", tenantID, "user_id", cfg.AutoLinkUserID)
		return
	}

	contacts, err := c.store.GetContactsBySenderIDs(ctx, []string{senderID})
	if err != nil {
		slog.Warn("contact_collector.auto_link.contact_lookup_failed",
			"error", err, "sender_id", senderID)
		return
	}
	contact, ok := contacts[senderID]
	if !ok {
		return // contact disappeared between upsert and lookup — retry next message
	}

	if err := c.store.MergeContacts(ctx, []uuid.UUID{contact.ID}, tu.ID); err != nil {
		slog.Warn("contact_collector.auto_link.merge_failed",
			"error", err, "contact_id", contact.ID, "tenant_user_id", tu.ID)
		return
	}
	slog.Info("contact_collector.auto_link.ok",
		"channel_type", channelType,
		"channel_instance", channelInstance,
		"sender_id", senderID,
		"tenant_user_id", tu.ID,
		"tenant_user_user_id", cfg.AutoLinkUserID,
	)
}
