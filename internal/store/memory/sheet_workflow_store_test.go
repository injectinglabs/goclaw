package memory

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// TestListAllCells_DeterministicOrder asserts the memory impl matches the
// PG impl's contract: ListAllCells returns every cell for a run regardless
// of status, sorted by (row_idx, col_idx). The deterministic order is what
// lets the SPA workflow.runState caller fold the snapshot into a reducer
// that asserts (row,col) uniqueness without a secondary sort.
func TestListAllCells_DeterministicOrder(t *testing.T) {
	s := NewSheetWorkflowStore()
	runID := uuid.New()

	// Init 3×2 grid + selectively transition cells through all states.
	if err := s.BulkInitCells(context.Background(), runID, 3, 2); err != nil {
		t.Fatalf("BulkInitCells: %v", err)
	}

	// row=0,col=0 → done
	if err := s.UpdateCellStatus(context.Background(), runID, 0, 0, "done", nil, 1, 100, 50, intPtr(120)); err != nil {
		t.Fatalf("UpdateCellStatus done: %v", err)
	}
	// row=0,col=1 → error
	errMsg := "rate limit"
	if err := s.UpdateCellStatus(context.Background(), runID, 0, 1, "error", &errMsg, 3, 0, 0, nil); err != nil {
		t.Fatalf("UpdateCellStatus error: %v", err)
	}
	// row=1,col=0 → running
	if err := s.UpdateCellStatus(context.Background(), runID, 1, 0, "running", nil, 1, 0, 0, nil); err != nil {
		t.Fatalf("UpdateCellStatus running: %v", err)
	}
	// row=2,col=1 → skipped
	if err := s.UpdateCellStatus(context.Background(), runID, 2, 1, "skipped", nil, 0, 0, 0, nil); err != nil {
		t.Fatalf("UpdateCellStatus skipped: %v", err)
	}
	// row=1,col=1 and row=2,col=0 stay queued (default after BulkInitCells)

	cells, err := s.ListAllCells(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListAllCells: %v", err)
	}

	if len(cells) != 6 {
		t.Fatalf("expected 6 cells (3 rows × 2 cols), got %d", len(cells))
	}

	// Assert deterministic (row,col) order.
	want := []struct {
		row, col int
		status   string
	}{
		{0, 0, "done"},
		{0, 1, "error"},
		{1, 0, "running"},
		{1, 1, "queued"},
		{2, 0, "queued"},
		{2, 1, "skipped"},
	}
	for i, w := range want {
		got := cells[i]
		if got.RowIdx != w.row || got.ColIdx != w.col {
			t.Errorf("cells[%d]: position (%d,%d) want (%d,%d)", i, got.RowIdx, got.ColIdx, w.row, w.col)
		}
		if got.Status != w.status {
			t.Errorf("cells[%d] @(%d,%d): status %q want %q", i, w.row, w.col, got.Status, w.status)
		}
	}

	// Spot-check that per-cell metadata round-trips correctly.
	for _, c := range cells {
		if c.RowIdx == 0 && c.ColIdx == 0 {
			if c.TokensIn != 100 || c.TokensOut != 50 {
				t.Errorf("(0,0) tokens: in=%d out=%d, want 100/50", c.TokensIn, c.TokensOut)
			}
			if c.LatencyMs == nil || *c.LatencyMs != 120 {
				t.Errorf("(0,0) latency: %v, want 120", c.LatencyMs)
			}
		}
		if c.RowIdx == 0 && c.ColIdx == 1 {
			if c.Attempt != 3 {
				t.Errorf("(0,1) attempt: %d, want 3", c.Attempt)
			}
			if c.ErrorMessage == nil || *c.ErrorMessage != "rate limit" {
				t.Errorf("(0,1) error_message: %v, want 'rate limit'", c.ErrorMessage)
			}
		}
	}
}

// TestListAllCells_UnknownRun_ReturnsNil asserts the contract for an
// unknown run id: nil slice + nil error (caller treats nil/empty
// identically). The workflow.runState handler relies on this — it shows
// an empty grid rather than a 500 when the run was already evicted.
func TestListAllCells_UnknownRun_ReturnsNil(t *testing.T) {
	s := NewSheetWorkflowStore()

	cells, err := s.ListAllCells(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ListAllCells unknown run: %v", err)
	}
	if cells != nil {
		t.Errorf("unknown run should return nil, got %d cells", len(cells))
	}
}

// TestListAllCells_AssignableToInterface ensures the in-memory impl
// stays interface-compatible with store.SheetWorkflowStore. Without
// this, adding the method to the interface but forgetting to add to
// the impl would compile (the interface is satisfied structurally) —
// this catches the symmetric mistake.
func TestListAllCells_AssignableToInterface(t *testing.T) {
	var _ store.SheetWorkflowStore = NewSheetWorkflowStore()
}

func intPtr(n int) *int { return &n }
