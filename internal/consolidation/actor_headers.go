package consolidation

import (
	"context"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/actorheaders"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// attachBackgroundActorHeaders wraps ctx with the X-Actor-User-ID +
// X-Actor-Org-ID HTTP headers that web-agent-api requires on the
// service-token path. The actual logic (load tenant, prefer
// external_org_id, fall back to slug) lives in the shared
// `internal/actorheaders` package — kept here as a one-line shim so
// existing call sites read naturally.
func attachBackgroundActorHeaders(ctx context.Context, ts store.TenantStore, tenantID uuid.UUID, userID string) context.Context {
	return actorheaders.Attach(ctx, ts, tenantID, userID)
}
