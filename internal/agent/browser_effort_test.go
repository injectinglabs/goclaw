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

func TestBrowserTurnEffort(t *testing.T) {
	ext := &RunRequest{ClientKind: "extension"}
	web := &RunRequest{ClientKind: "website"}

	// Non-extension run: unchanged.
	if got := browserTurnEffort(web, mkState(5), "high"); got != "high" {
		t.Errorf("website run should keep high, got %q", got)
	}
	// Extension, planning turn (iter 0): full reasoning but capped at medium.
	if got := browserTurnEffort(ext, mkState(0), "high"); got != "medium" {
		t.Errorf("planning turn: want medium (capped), got %q", got)
	}
	// Extension, routine mid-task turn: minimal.
	if got := browserTurnEffort(ext, mkState(7), "high"); got != "low" {
		t.Errorf("mechanical turn: want low, got %q", got)
	}
	// Extension, recovery after a tool error: keep reasoning (capped medium).
	errTurn := mkState(7, providers.Message{Role: "tool", IsError: true, Content: "boom"})
	if got := browserTurnEffort(ext, errTurn, "high"); got != "medium" {
		t.Errorf("recovery (error): want medium, got %q", got)
	}
	// Extension, recovery after a loop warning.
	warnTurn := mkState(9, providers.Message{Role: "user", Content: "[System: WARNING — execute_action repeated]"})
	if got := browserTurnEffort(ext, warnTurn, "high"); got != "medium" {
		t.Errorf("recovery (warning): want medium, got %q", got)
	}
	// Configured already low: stays low even on planning turn.
	if got := browserTurnEffort(ext, mkState(0), "low"); got != "low" {
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
