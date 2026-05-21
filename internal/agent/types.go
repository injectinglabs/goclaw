package agent

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// Agent is the core abstraction for an AI agent execution loop.
// Implemented by *Loop; extracted as an interface for testability and composability.
type Agent interface {
	ID() string
	UUID() uuid.UUID
	OtherConfig() json.RawMessage
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
	IsRunning() bool
	Model() string
	ProviderName() string
	Provider() providers.Provider
	// ExternalOrgID and TenantSlug let out-of-loop call sites (e.g. the
	// auto-title goroutine in gateway/methods/chat.go) attach the same
	// X-Actor-Org-ID actor header that Loop.injectContext applies on the
	// regular run path. Prefer ExternalOrgID; fall back to TenantSlug
	// during the rollout window when auth-proxy hasn't stamped
	// tenants.settings.external_org_id yet.
	ExternalOrgID() string
	TenantSlug() string
}
