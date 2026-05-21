// Package actorheaders wraps a context with the X-Actor-User-ID +
// X-Actor-Org-ID HTTP headers that downstream service-token receivers
// (web-agent-api) require for attribution.
//
// Background: outbound `provider.Chat` calls from goclaw go to web-agent-api
// with `LLM_INTERNAL_AUTH_TOKEN` (a service token, NOT a user api_key).
// The receiver rejects the request with HTTP 400 unless both X-Actor-*
// headers are present and resolve to a valid (user, org) pair.
//
// This package centralises the "load tenant → prefer external_org_id →
// fall back to slug → write providers.WithActorHeaders" pattern so each
// background worker (consolidation, vault enrich, history compaction)
// doesn't reinvent it.
package actorheaders

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Attach wraps ctx with X-Actor-User-ID + X-Actor-Org-ID for the next
// outbound `provider.Chat` call.
//
// Org ID preference:
//  1. `tenants.settings.external_org_id` (the web-backend `organizations.id`
//     UUID stamped by auth-proxy on login). Canonical.
//  2. `tenants.slug` (rollout-window fallback for tenants the auth-proxy
//     hasn't stamped yet; receiver inverts via `_goclaw_slug_to_web_slug`).
//
// Use this when the call has a real human user in scope (extension chat,
// Telegram inbound, integration sync triggered by a Connect click). For
// background work without a specific user (vault bulk rescan, legacy
// channel compaction) use `AttachInfra` instead — it sends
// `X-Actor-Source: infra` so the receiver charges the org without
// touching any user's monthly quota.
//
// Tolerant to nil/empty inputs — returns ctx unchanged rather than failing
// the caller.
func Attach(ctx context.Context, ts store.TenantStore, tenantID uuid.UUID, userID string) context.Context {
	if ts == nil || userID == "" || tenantID == uuid.Nil {
		return ctx
	}
	orgID := resolveOrgID(ctx, ts, tenantID)
	if orgID == "" {
		return ctx
	}
	return providers.WithActorHeaders(ctx, map[string]string{
		"X-Actor-User-ID": userID,
		"X-Actor-Org-ID":  orgID,
	})
}

// AttachInfra wraps ctx with X-Actor-Org-ID + X-Actor-Source: infra for
// the next outbound `provider.Chat` call. Used by background paths that
// don't have a real user in scope (vault bulk rescan, legacy channel
// compaction, future scheduled jobs).
//
// The receiver (web-agent-api) recognises `X-Actor-Source: infra` on the
// service-token path: it skips per-user quota gates entirely and writes
// the ai_tasks row with source='infra' and NULL user_id. The org still
// pays — operators can see infra cost per org in the dashboard — but no
// single user's monthly budget is touched, which is the right billing
// model for system-initiated work.
//
// X-Actor-User-ID is intentionally NOT sent on this path. If a caller
// has a meaningful user, they should use `Attach`; if they don't, they
// shouldn't fabricate one (would have charged a real user's quota for
// background work, which is the bug this design avoids).
func AttachInfra(ctx context.Context, ts store.TenantStore, tenantID uuid.UUID) context.Context {
	if ts == nil || tenantID == uuid.Nil {
		return ctx
	}
	orgID := resolveOrgID(ctx, ts, tenantID)
	if orgID == "" {
		return ctx
	}
	return providers.WithActorHeaders(ctx, map[string]string{
		"X-Actor-Org-ID": orgID,
		"X-Actor-Source": "infra",
	})
}

// resolveOrgID looks up the org identifier the receiver expects on
// X-Actor-Org-ID: preferring the UUID stamped on tenants.settings.
// external_org_id, falling back to the slug for tenants the auth-proxy
// hasn't touched yet. Empty result means "give up, don't send actor
// headers" — the call will 400 at the receiver but that's the loud
// signal we want.
func resolveOrgID(ctx context.Context, ts store.TenantStore, tenantID uuid.UUID) string {
	tenant, err := ts.GetTenant(ctx, tenantID)
	if err != nil || tenant == nil {
		return ""
	}
	if len(tenant.Settings) > 0 {
		var s struct {
			ExternalOrgID string `json:"external_org_id"`
		}
		if json.Unmarshal(tenant.Settings, &s) == nil && s.ExternalOrgID != "" {
			return s.ExternalOrgID
		}
	}
	return tenant.Slug
}
