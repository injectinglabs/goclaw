package agent

import (
	"context"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// In multi-tenant mode the Loop's global provider/model are nil/empty and the
// real tenant-scoped provider arrives per-run via req.ProviderOverride /
// req.ModelOverride (see cmd/gateway_agents.go — "no global primary provider").
// resolveRunProviderModel must prefer those overrides over the nil global.
func TestResolveRunProviderModel_PrefersRequestOverride(t *testing.T) {
	l := &Loop{provider: nil, model: ""} // multi-tenant: no global provider

	// nil req / no override → falls back to the (nil) global.
	if p, m := l.resolveRunProviderModel(nil); p != nil || m != "" {
		t.Fatalf("nil req: want (nil, \"\"), got (%v, %q)", p, m)
	}

	// Per-run override present → used instead of the nil global.
	override := &stubProvider{response: "x"}
	req := &RunRequest{ProviderOverride: override, ModelOverride: "tenant-model"}
	p, m := l.resolveRunProviderModel(req)
	if p != override || m != "tenant-model" {
		t.Fatalf("override: want (override, tenant-model), got (%v, %q)", p, m)
	}
}

// Regression: rescueEmptyReply used l.provider/l.model directly, so on a
// multi-tenant deploy (nil global provider) it bailed at the guard and never
// retried — every empty model turn fell straight through to the
// MsgEmptyReplyFallback sentence. It must now run via the per-run provider.
func TestRescueEmptyReply_UsesPerRunProviderWhenGlobalNil(t *testing.T) {
	l := &Loop{provider: nil, model: ""} // multi-tenant
	history := []providers.Message{{Role: "user", Content: "hi"}}

	// No per-run provider → rescue genuinely can't run → empty.
	if got := l.rescueEmptyReply(context.Background(), &RunRequest{}, history, nil); got != "" {
		t.Fatalf("no provider: want empty, got %q", got)
	}

	// With the tenant-scoped provider the rescue runs and returns its text.
	req := &RunRequest{ProviderOverride: &stubProvider{response: "rescued answer"}, ModelOverride: "m"}
	if got := l.rescueEmptyReply(context.Background(), req, history, nil); got != "rescued answer" {
		t.Fatalf("with override: want %q, got %q", "rescued answer", got)
	}
}
