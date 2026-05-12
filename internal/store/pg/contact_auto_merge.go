package pg

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// TryAutoMergeContact links a freshly-upserted channel_contact to the
// channel_instance owner's tenant_user when conditions are safe.
//
// Motivation: a Telegram bot is connected from the web (where we know the
// caller's Cognito sub) via the `connect_telegram` MCP tool. The bot then
// receives an inbound message in goclaw and an `EnsureContact` upserts the
// channel_contact row with `sender_id = <Telegram numeric ID>` and
// `merged_id = NULL`. Without this link the downstream
// `ResolveTenantUserID` returns "", goclaw keeps the Telegram numeric ID
// as `session.user_id`, and every MCP tool call (`gmail_status`,
// `gmail_get_connect_url`, ...) hits web-backend with a non-UUID `user_id`
// and crashes with `invalid input syntax for type uuid`.
//
// `TryAutoMergeContact` closes the gap by promoting the new contact to the
// owner's tenant_user automatically. Conditions:
//
//   1. Contact exists for (tenant, channel_type, sender_id) and is not yet
//      merged. Already-linked contacts are a no-op (this is fired on every
//      inbound message via ContactCollector, so it must be cheap).
//   2. The `channel_instances` row carries `created_by` (the Cognito sub of
//      whoever ran `connect_telegram` in the web). Default-seeded rows have
//      no `created_by` and stay untouched.
//   3. **No other contact in the same channel_instance is already merged.**
//      Once any user has claimed the bot, we stop blindly promoting new
//      senders — a stranger who finds the bot link must go through an
//      explicit link flow rather than impersonate the owner.
//
// Isolation note: each user connects their own bot via `connect_telegram`,
// which sets `config.allow_from = [their_telegram_user]` (DM policy
// "allowlist"). Telegram messages from any other Telegram account are
// rejected upstream in `internal/channels/telegram/handlers.go::handleMessage`
// before they ever reach `ContactCollector.EnsureContact`. That means by
// the time we get here, the sender has already been verified as the
// `created_by` owner of this specific bot. We don't need to re-check
// `allow_from` — the absence of cross-user pollution is structural.
//
// The merge target is the tenant_user matching (tenant_id, user_id =
// created_by). We create the tenant_user row on the fly (idempotent UPSERT)
// if it doesn't exist yet — same pattern as `handleMergeContacts.create_user`.
//
// Non-fatal: any error here logs and returns nil so message processing
// continues even if the auto-link doesn't take. A failure here just means
// the user keeps seeing the old 500 on integration tools until they re-run
// `link_telegram_profile` manually — never a worse state than before this
// method existed.
func (s *PGContactStore) TryAutoMergeContact(ctx context.Context, channelType, channelInstance, senderID string) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil || channelType == "" || channelInstance == "" || senderID == "" {
		return nil
	}

	// 1. Locate the contact, and bail if already merged.
	var contactID uuid.UUID
	var existing sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, merged_id
		  FROM channel_contacts
		 WHERE tenant_id = $1
		   AND channel_type = $2
		   AND sender_id = $3
		   AND COALESCE(thread_id, '') = ''
		 LIMIT 1
	`, tenantID, channelType, senderID).Scan(&contactID, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		slog.Warn("contacts.auto_merge.find_contact_failed", "error", err,
			"tenant_id", tenantID, "channel_type", channelType, "sender_id", senderID)
		return nil
	}
	if existing.Valid && existing.String != "" {
		return nil // Already linked.
	}

	// 2. Look up the channel_instance owner. No owner → no auto-merge target.
	var createdBy string
	err = s.db.QueryRowContext(ctx, `
		SELECT created_by
		  FROM channel_instances
		 WHERE tenant_id = $1
		   AND channel_type = $2
		   AND name = $3
		   AND created_by IS NOT NULL
		   AND created_by != ''
		 LIMIT 1
	`, tenantID, channelType, channelInstance).Scan(&createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		slog.Warn("contacts.auto_merge.find_instance_failed", "error", err,
			"tenant_id", tenantID, "channel_type", channelType, "channel_instance", channelInstance)
		return nil
	}

	// 3. Safety guard: don't auto-promote when someone else has already
	// claimed the bot. Once the channel has any merged contact, new senders
	// must be linked explicitly (or by an admin) rather than silently
	// inheriting the owner's identity.
	var otherMerged int
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM channel_contacts
		 WHERE tenant_id = $1
		   AND channel_type = $2
		   AND channel_instance = $3
		   AND merged_id IS NOT NULL
		   AND id <> $4
	`, tenantID, channelType, channelInstance, contactID).Scan(&otherMerged)
	if err != nil {
		slog.Warn("contacts.auto_merge.count_merged_failed", "error", err,
			"tenant_id", tenantID, "channel_instance", channelInstance)
		return nil
	}
	if otherMerged > 0 {
		slog.Debug("contacts.auto_merge.skipped_channel_already_claimed",
			"tenant_id", tenantID, "channel_instance", channelInstance,
			"sender_id", senderID, "other_merged", otherMerged)
		return nil
	}

	// 4. Upsert the tenant_user for (tenant, created_by). We can't use the
	// tenant_store directly without importing a cycle, so inline the same
	// INSERT … ON CONFLICT DO UPDATE RETURNING that CreateTenantUserReturning
	// uses (pg/tenant_store.go:141). Role 'member' is the safe default;
	// auth-proxy is the source of truth for promoting to owner/admin.
	var tenantUserID uuid.UUID
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO tenant_users (id, tenant_id, user_id, role, created_at, updated_at)
		VALUES ($1, $2, $3, 'member', NOW(), NOW())
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET updated_at = NOW()
		RETURNING id
	`, store.GenNewID(), tenantID, createdBy).Scan(&tenantUserID)
	if err != nil {
		slog.Warn("contacts.auto_merge.upsert_tenant_user_failed", "error", err,
			"tenant_id", tenantID, "user_id", createdBy)
		return nil
	}

	// 5. Promote the contact. ResolveTenantUserID cache must be invalidated
	// so the very next inbound message picks up the new link instead of the
	// stale 60-second "not merged" miss.
	_, err = s.db.ExecContext(ctx, `
		UPDATE channel_contacts
		   SET merged_id = $1
		 WHERE id = $2 AND merged_id IS NULL
	`, tenantUserID, contactID)
	if err != nil {
		slog.Warn("contacts.auto_merge.update_failed", "error", err,
			"tenant_id", tenantID, "contact_id", contactID, "tenant_user_id", tenantUserID)
		return nil
	}
	s.InvalidateContactResolveCache()

	slog.Info("contacts.auto_merged",
		"tenant_id", tenantID,
		"channel_type", channelType,
		"channel_instance", channelInstance,
		"sender_id", senderID,
		"tenant_user_id", tenantUserID,
		"linked_to_user_id", createdBy,
	)
	return nil
}
