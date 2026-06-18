package agent

import (
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

func mkState(iter int, msgs ...providers.Message) *pipeline.RunState {
	s := &pipeline.RunState{Iteration: iter, Messages: pipeline.NewMessageBuffer(providers.Message{})}
	for _, m := range msgs {
		s.Messages.AppendPending(m)
	}
	return s
}

func TestTurnEffort(t *testing.T) {
	ext := &RunRequest{ClientKind: "extension"}
	web := &RunRequest{ClientKind: "website"}

	// Routine mid-task turn drops to "low" for ALL runs (web and extension).
	if got := turnEffort(web, mkState(5), "high"); got != "low" {
		t.Errorf("web routine turn: want low, got %q", got)
	}
	if got := turnEffort(ext, mkState(7), "high"); got != "low" {
		t.Errorf("extension routine turn: want low, got %q", got)
	}
	// Planning turn (iter 0): full reasoning but capped at medium — both kinds.
	if got := turnEffort(web, mkState(0), "high"); got != "medium" {
		t.Errorf("web planning turn: want medium (capped), got %q", got)
	}
	if got := turnEffort(ext, mkState(0), "high"); got != "medium" {
		t.Errorf("extension planning turn: want medium (capped), got %q", got)
	}
	// Recovery after a tool error: keep reasoning (capped medium).
	errTurn := mkState(7, providers.Message{Role: "tool", IsError: true, Content: "boom"})
	if got := turnEffort(web, errTurn, "high"); got != "medium" {
		t.Errorf("recovery (error): want medium, got %q", got)
	}
	// Recovery after a loop warning.
	warnTurn := mkState(9, providers.Message{Role: "user", Content: "[System: WARNING — execute_action repeated]"})
	if got := turnEffort(web, warnTurn, "high"); got != "medium" {
		t.Errorf("recovery (warning): want medium, got %q", got)
	}
	// Configured already low: stays low even on planning turn (never raised).
	if got := turnEffort(web, mkState(0), "low"); got != "low" {
		t.Errorf("low config should pass through, got %q", got)
	}
}

func TestEffectiveMaxIterations(t *testing.T) {
	l := &Loop{maxIterations: 30}
	ext := &RunRequest{ClientKind: "extension"}
	web := &RunRequest{ClientKind: "website"}

	if got := l.effectiveMaxIterations(web); got != 30 {
		t.Errorf("website run keeps agent default 30, got %d", got)
	}
	if got := l.effectiveMaxIterations(ext); got != browserMaxIterations {
		t.Errorf("extension run raised to %d, got %d", browserMaxIterations, got)
	}
	// per-request value may only LOWER.
	if got := l.effectiveMaxIterations(&RunRequest{ClientKind: "extension", MaxIterations: 50}); got != 50 {
		t.Errorf("req override should lower to 50, got %d", got)
	}
	// agent already above the browser floor stays as-is.
	l2 := &Loop{maxIterations: 200}
	if got := l2.effectiveMaxIterations(ext); got != 200 {
		t.Errorf("agent default 200 should be kept, got %d", got)
	}
}
