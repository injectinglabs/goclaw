package agent

import (
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
)

// An agent with no explicit reasoning effort (ParseReasoningConfig → "off")
// must fall to the bounded default, so thinking models get a per-turn cap
// instead of running at max thinking. An explicit effort passes through.
func TestEffectiveReasoningEffort(t *testing.T) {
	if got := effectiveReasoningEffort(""); got != defaultReasoningEffort {
		t.Fatalf("empty: got %q, want default %q", got, defaultReasoningEffort)
	}
	if got := effectiveReasoningEffort("off"); got != defaultReasoningEffort {
		t.Fatalf("off: got %q, want default %q", got, defaultReasoningEffort)
	}
	if got := effectiveReasoningEffort("high"); got != "high" {
		t.Fatalf("explicit high must pass through, got %q", got)
	}
	// The default must be a concrete bounded level — never empty/off, or the
	// point (engaging turnEffort) is defeated.
	if defaultReasoningEffort == "" || defaultReasoningEffort == "off" {
		t.Fatalf("defaultReasoningEffort must be concrete, got %q", defaultReasoningEffort)
	}
}

// turnEffort caps the planning turn at "medium" — so even if the default were
// "high", a thinking model never gets max reasoning forced on it.
func TestTurnEffort_CapsPlanningTurn(t *testing.T) {
	planning := &pipeline.RunState{}
	planning.Iteration = 0 // planning turn → recentTroubleSignal is short-circuited
	if got := turnEffort(nil, planning, "high"); got != "medium" {
		t.Fatalf("planning turn: high must cap to medium, got %q", got)
	}
}
