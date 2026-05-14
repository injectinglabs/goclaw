package consolidation

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// attachBackgroundActorHeaders wraps ctx with X-Actor-User-ID and
// X-Actor-Org-ID for the outbound provider.Chat call that the
// consolidation workers (episodic, dreaming, semantic) make against
// the per-tenant LLM provider — which on the web-agent stack is
// llm-service-web. Without these headers the provider's cached api_key
// (a SERVICE token since PR #84) attributes the background call to no
// one in particular, and web-agent-api 400's because it requires the
// X-Actor pair on the service-token path.
//
// Org ID resolution preference:
//  1. tenants.settings.external_org_id (UUID stamped by auth-proxy
//     on every login — canonical web-backend organizations.id).
//  2. tenants.slug (rollout fallback for tenants the auth-proxy
//     hasn't stamped yet; resolve_actor_payload on web-agent-api
//     inverts the slug via _goclaw_slug_to_web_slug).
//
// All inputs are tolerated nil/empty: returns ctx unchanged when any
// required field is missing rather than failing the worker. The cost
// of an extra GetTenant per consolidation tick is negligible — these
// fire at most a few times per session per user.
func attachBackgroundActorHeaders(ctx context.Context, ts store.TenantStore, tenantID uuid.UUID, userID string) context.Context {
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
