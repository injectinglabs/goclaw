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

// capturingProvider records the last ChatRequest so a test can assert which
// options the caller set.
type capturingProvider struct {
	response string
	lastReq  providers.ChatRequest
}

func (c *capturingProvider) Chat(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	c.lastReq = req
	return &providers.ChatResponse{Content: c.response}, nil
}

func (c *capturingProvider) ChatStream(_ context.Context, req providers.ChatRequest, _ func(providers.StreamChunk)) (*providers.ChatResponse, error) {
	c.lastReq = req
	return &providers.ChatResponse{Content: c.response}, nil
}
func (c *capturingProvider) DefaultModel() string { return "cap-model" }
func (c *capturingProvider) Name() string         { return "cap" }

// Universal empty-reply fix: the rescue must constrain the reasoning budget
// (OptThinkingLevel) AND request a generous, auto-clamped output budget
// (OptMaxTokens), so a thinking model that starved its visible-text budget on
// the primary turn gets room to actually emit the answer on retry. These are
// model-agnostic (thinking level is gated to thinking routes; max_tokens
// clamps per model), so the fix holds for any provider.
func TestRescueEmptyReply_ConstrainsThinkingAndRaisesOutputBudget(t *testing.T) {
	cap := &capturingProvider{response: "answer"}
	l := &Loop{provider: nil, model: ""}
	req := &RunRequest{ProviderOverride: cap, ModelOverride: "m"}
	history := []providers.Message{{Role: "user", Content: "hi"}}

	if got := l.rescueEmptyReply(context.Background(), req, history, nil); got != "answer" {
		t.Fatalf("want %q, got %q", "answer", got)
	}
	if lvl, _ := cap.lastReq.Options[providers.OptThinkingLevel].(string); lvl != "low" {
		t.Fatalf("rescue must cap reasoning: OptThinkingLevel = %q, want \"low\"", lvl)
	}
	if mt, _ := cap.lastReq.Options[providers.OptMaxTokens].(int); mt != rescueMaxOutputTokens {
		t.Fatalf("rescue must request a generous budget: OptMaxTokens = %d, want %d", mt, rescueMaxOutputTokens)
	}
	if strip, _ := cap.lastReq.Options[providers.OptStripThinking].(bool); !strip {
		t.Fatalf("rescue must still strip thinking from the reply")
	}
}
