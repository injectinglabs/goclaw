package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// TestActiveSessionsForUser_FiltersAbortingRuns verifies that runs whose
// teardown has begun (State==1) are excluded from ActiveSessionsForUser.
// This is the server side of the reload-hang fix: a WS reconnecting
// between MarkAborting and UnregisterRun must NOT see the dying run and
// subscribe to a runID that will never emit another event.
func TestActiveSessionsForUser_FiltersAbortingRuns(t *testing.T) {
	r := NewRouter()
	tenantID := uuid.New()
	userID := "user-A"

	ctx := store.WithTenantID(context.Background(), tenantID)
	_, cancel := context.WithCancel(ctx)
	r.RegisterRun(ctx, "run-1", "sess-1", "agent-1", userID, cancel)

	// Pre-condition: ActiveSessionsForUser returns the live run.
	if got := r.ActiveSessionsForUser(tenantID, userID); len(got) != 1 {
		t.Fatalf("expected 1 active session before MarkAborting, got %d", len(got))
	}

	r.MarkAborting("run-1")

	// Post-condition: filtered out even though still in activeRuns map.
	if got := r.ActiveSessionsForUser(tenantID, userID); len(got) != 0 {
		t.Fatalf("expected 0 sessions after MarkAborting, got %d", len(got))
	}

	// Sanity: run is still in the map (UnregisterRun not yet called).
	if _, ok := r.activeRuns.Load("run-1"); !ok {
		t.Fatalf("MarkAborting must not delete the run; UnregisterRun does that")
	}
}

// TestMarkAborting_OnlyFlipsRunningRuns guards against State regressions
// (e.g. State 2 "done" being clobbered back to 1).
func TestMarkAborting_OnlyFlipsRunningRuns(t *testing.T) {
	r := NewRouter()
	_, cancel := context.WithCancel(context.Background())
	r.RegisterRun(context.Background(), "run-x", "sess-x", "agent", "uid", cancel)

	val, _ := r.activeRuns.Load("run-x")
	run := val.(*ActiveRun)
	run.State.Store(2) // simulate "done"

	r.MarkAborting("run-x")
	if got := run.State.Load(); got != 2 {
		t.Fatalf("MarkAborting must not move State from 2 back to 1, got %d", got)
	}
}

// TestMarkAborting_UnknownRunIDIsNoOp ensures we don't panic on a stale
// runID (race between AbortRun, partial-save defer and UnregisterRun).
func TestMarkAborting_UnknownRunIDIsNoOp(t *testing.T) {
	r := NewRouter()
	r.MarkAborting("never-existed") // must not panic
}
