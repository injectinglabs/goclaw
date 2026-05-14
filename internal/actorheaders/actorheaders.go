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
// Tolerant to nil/empty inputs — returns ctx unchanged rather than failing
// the caller. The cost of the extra GetTenant lookup is amortised: callers
// fire once per worker iteration, which happens at most a handful of
// times per session per user.
func Attach(ctx context.Context, ts store.TenantStore, tenantID uuid.UUID, userID string) context.Context {
	if ts == nil || userID == "" || tenantID == uuid.Nil {
		return ctx
	}
	tenant, err := ts.GetTenant(ctx, tenantID)
	if err != nil || tenant == nil {
		return ctx
	}
	orgID := ""
	if len(tenant.Settings) > 0 {
		var s struct {
			ExternalOrgID string `json:"external_org_id"`
		}
		if json.Unmarshal(tenant.Settings, &s) == nil {
			orgID = s.ExternalOrgID
		}
	}
	if orgID == "" {
		orgID = tenant.Slug
	}
	if orgID == "" {
		return ctx
	}
	return providers.WithActorHeaders(ctx, map[string]string{
		"X-Actor-User-ID": userID,
		"X-Actor-Org-ID":  orgID,
	})
}
