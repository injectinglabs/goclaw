package mcp

import (
	"context"

	"github.com/google/uuid"
)

// Per-call X-Actor-* headers for outbound MCP HTTP calls
// ─────────────────────────────────────────────────────────
//
// Internal sidecars (document-mcp, future workers) need the calling
// tenant + user identity to scope their work — for example, document-mcp
// uses these to label S3 objects under {tenant_uuid}/{user_uuid}/created/.
// Bake-at-connect headers from mcp_servers.headers don't work for this:
// X-Actor-User-ID changes per call (different chat users share one MCP
// pool connection), and X-Actor-Org-ID changes per call too on team
// accounts where multiple members talk to the same agent.
//
// Solution: use mcp-go's WithHTTPHeaderFunc — invoked per request with
// the request context — and read the actor IDs we stashed in ctx
// immediately before invoking the BridgeTool. Same pattern goclaw already
// uses for outbound /v1/chat/completions (see external_org_id attribution
// in llm-service-web).

type actorCtxKey int

const (
	actorUserIDKey actorCtxKey = iota
	actorOrgIDKey
)

// WithActorIdentity returns a new context carrying the actor (user + org)
// that the MCP HTTP header function will surface as X-Actor-User-ID and
// X-Actor-Org-ID on the next outbound call. Empty UUIDs are skipped so
// the downstream sidecar can apply its own "anonymous" fallback.
func WithActorIdentity(ctx context.Context, userID, orgID uuid.UUID) context.Context {
	if userID != uuid.Nil {
		ctx = context.WithValue(ctx, actorUserIDKey, userID)
	}
	if orgID != uuid.Nil {
		ctx = context.WithValue(ctx, actorOrgIDKey, orgID)
	}
	return ctx
}

// actorHeadersFromContext extracts the per-call X-Actor-* headers from
// ctx. Returns an empty map when neither id is present — the caller (the
// mcp-go HTTPHeaderFunc closure) is expected to merge this on top of the
// static server-configured headers, so missing values just leave the
// static map untouched.
func actorHeadersFromContext(ctx context.Context) map[string]string {
	out := map[string]string{}
	if v, ok := ctx.Value(actorUserIDKey).(uuid.UUID); ok && v != uuid.Nil {
		out["X-Actor-User-ID"] = v.String()
	}
	if v, ok := ctx.Value(actorOrgIDKey).(uuid.UUID); ok && v != uuid.Nil {
		out["X-Actor-Org-ID"] = v.String()
	}
	return out
}
