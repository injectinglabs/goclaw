package mcp

import (
	"context"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// Per-call X-Actor-* headers for outbound MCP HTTP calls
// ─────────────────────────────────────────────────────────
//
// Internal sidecars (document-mcp, future workers) need the calling
// tenant + user identity to scope their work — and document-mcp also
// forwards them to downstream service-token receivers (llm-service-web
// → web-agent-api) for billing attribution.
//
// Source of truth: the agent loop already calls
// `providers.WithActorHeaders(ctx, …)` in loop_context.go before any
// tool runs, with the correct X-Actor-Org-ID resolution (prefer
// `tenants.settings.external_org_id`, fall back to tenant slug). The
// MCP bridge therefore reads from the same context map instead of
// duplicating that logic — keeps the external_org_id/slug source of
// truth in one place and avoids the previous bug where the bridge
// forwarded goclaw's internal tenant UUID (rejected by web-agent-api
// with "Actor is not an active member of the claimed org").
//
// Legacy UUID-keyed fallback: `WithActorIdentity` is still exported
// for the few code paths that build a context outside the agent loop
// (background workers attach actor headers via
// `internal/actorheaders.Attach` which already does The Right Thing,
// but defensive fallback is cheap insurance).

type actorCtxKey int

const (
	actorUserIDKey actorCtxKey = iota
	actorOrgIDKey
)

// WithActorIdentity is the legacy fallback path. Prefer building the
// context with `actorheaders.Attach` (or `providers.WithActorHeaders`
// directly) which produces the slug/external_org_id the receiver
// expects. Kept exported so call sites that haven't migrated still
// surface an identity; the value is only consulted when the
// providers' map is empty.
func WithActorIdentity(ctx context.Context, userID, orgID uuid.UUID) context.Context {
	if userID != uuid.Nil {
		ctx = context.WithValue(ctx, actorUserIDKey, userID)
	}
	if orgID != uuid.Nil {
		ctx = context.WithValue(ctx, actorOrgIDKey, orgID)
	}
	return ctx
}

// actorHeadersFromContext is the mcp-go HTTPHeaderFunc invoked per
// outbound HTTP request. Prefers `providers.ActorHeadersFromCtx` (set
// by the agent loop with correct external_org_id resolution) and
// falls back to the legacy UUID-keyed store written by
// `WithActorIdentity` only when the providers' map has no entries
// for the corresponding header.
func actorHeadersFromContext(ctx context.Context) map[string]string {
	out := map[string]string{}

	// Preferred path: headers stashed by the agent loop /
	// actorheaders.Attach. These are already in the format the
	// downstream receiver expects (slug or external_org_id UUID).
	for k, v := range providers.ActorHeadersFromCtx(ctx) {
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}

	// Fallback: legacy UUID-keyed store. Only fill keys the
	// providers' map didn't already set so the canonical resolution
	// always wins.
	if _, ok := out["X-Actor-User-ID"]; !ok {
		if v, ok2 := ctx.Value(actorUserIDKey).(uuid.UUID); ok2 && v != uuid.Nil {
			out["X-Actor-User-ID"] = v.String()
		}
	}
	if _, ok := out["X-Actor-Org-ID"]; !ok {
		if v, ok2 := ctx.Value(actorOrgIDKey).(uuid.UUID); ok2 && v != uuid.Nil {
			out["X-Actor-Org-ID"] = v.String()
		}
	}
	return out
}
