package mcp

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// Per-call X-Actor-* headers for outbound MCP HTTP calls
// ─────────────────────────────────────────────────────────
//
// Internal sidecars (document-mcp, future workers) need the calling
// tenant + user identity to scope their work — and document-mcp also
// forwards them to downstream service-token receivers (web-agent-api)
// for billing attribution.
//
// Source of truth: the agent loop calls
// `providers.WithActorHeaders(ctx, …)` in loop_context.go before any
// tool runs, with the correct X-Actor-Org-ID resolution (prefer
// `tenants.settings.external_org_id`, fall back to tenant slug). The
// MCP bridge reads from the same context map — keeps the
// external_org_id/slug source of truth in one place.
//
// History: a previous version of this file stashed UUIDs via a
// private `WithActorIdentity(userID, orgID uuid.UUID)` helper and
// produced X-Actor-Org-ID from `store.TenantIDFromContext(ctx)` —
// goclaw's *internal* tenant UUID. That value is meaningless to
// web-agent-api (which expects the web-backend organizations.id /
// slug) and produced HTTP 403 "Actor is not an active member of the
// claimed org" the moment any sidecar forwarded it. Path removed in
// the cleanup PR following PR #153.

// actorHeadersFromContext is the mcp-go HTTPHeaderFunc invoked per
// outbound HTTP request. Returns the actor headers map populated by
// the agent loop via `providers.WithActorHeaders`. Returns an empty
// map (not nil) when the caller did not attach headers — mcp-go
// merges this on top of static server-configured headers, so an
// empty map leaves the static map untouched.
func actorHeadersFromContext(ctx context.Context) map[string]string {
	out := map[string]string{}
	for k, v := range providers.ActorHeadersFromCtx(ctx) {
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}
