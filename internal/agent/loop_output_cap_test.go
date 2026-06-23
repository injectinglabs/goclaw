package agent

import (
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

type stubModelRegistry struct{ spec *providers.ModelSpec }

func (s *stubModelRegistry) Resolve(provider, modelID string) *providers.ModelSpec { return s.spec }
func (s *stubModelRegistry) Register(spec providers.ModelSpec)                      {}
func (s *stubModelRegistry) Catalog(provider string) []providers.ModelSpec          { return nil }

// The per-call output budget must not collapse to the 8192 Config fallback for
// models whose real ceiling is unknown — that starves thinking models (they
// spend the whole budget reasoning and emit zero text). Known models keep their
// real ceiling.
func TestResolveMaxOutputTokens(t *testing.T) {
	// No registry → generous floor.
	if got := (&Loop{}).resolveMaxOutputTokens("p", "m"); got != unresolvedMaxOutputTokens {
		t.Fatalf("nil registry: got %d, want %d", got, unresolvedMaxOutputTokens)
	}

	// Prod case: the "default" alias is registered from llm-service /v1/models,
	// which carries context_window but NO max_tokens → spec.MaxTokens == 0.
	// Must fall through to the floor, not return 0 (→ 8192 fallback → starvation).
	l := &Loop{modelRegistry: &stubModelRegistry{spec: &providers.ModelSpec{ContextWindow: 1048576, MaxTokens: 0}}}
	if got := l.resolveMaxOutputTokens("vertex", "default"); got != unresolvedMaxOutputTokens {
		t.Fatalf("MaxTokens=0 spec (default/gemini): got %d, want %d", got, unresolvedMaxOutputTokens)
	}

	// Known model with a real ceiling → use it (don't override / force a clamp).
	l = &Loop{modelRegistry: &stubModelRegistry{spec: &providers.ModelSpec{MaxTokens: 16000}}}
	if got := l.resolveMaxOutputTokens("anthropic", "claude-sonnet-4-6"); got != 16000 {
		t.Fatalf("real spec: got %d, want 16000", got)
	}
}
