package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	memstore "github.com/nextlevelbuilder/goclaw/internal/store/memory"
)

// recordingWriter captures every BatchWrite call. The orchestrator
// flushes once per wave, so for an N-wave workflow we expect N batches.
type recordingWriter struct {
	mu     sync.Mutex
	calls  [][]CellWrite
	failOn int // -1 = never; N = nth call (0-based) returns error
	err    error
}

func (w *recordingWriter) BatchWrite(_ context.Context, _ string, writes []CellWrite) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	callIdx := len(w.calls)
	cp := make([]CellWrite, len(writes))
	copy(cp, writes)
	w.calls = append(w.calls, cp)
	if w.failOn == callIdx && w.err != nil {
		return w.err
	}
	return nil
}

func (w *recordingWriter) snapshot() [][]CellWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]CellWrite, len(w.calls))
	for i, b := range w.calls {
		out[i] = append([]CellWrite(nil), b...)
	}
	return out
}

func TestSheetWriter_OneBatchPerWave(t *testing.T) {
	// 2 waves: [a] then [b dep on a]. 3 rows ⇒ 3 cells in each wave ⇒
	// 2 BatchWrite calls of 3 entries each.
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "A", Prompt: "p", Type: "text"},
		{ID: "b", Name: "B", Prompt: "p", Type: "text", DependsOn: []string{"a"}},
	})

	exec := &mockExecutor{behavior: func(t CellTask, _ int) (CellResult, error) {
		return CellResult{Value: t.Column.ID + ":" + intStr(t.RowIdx)}, nil
	}}
	bus := &recordingBus{}
	writer := &recordingWriter{failOn: -1}
	o := New(st, exec, bus, writer)

	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(3, "C"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.completed", 1, 2*time.Second) {
		t.Fatal("run did not complete")
	}

	calls := writer.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 batches (one per wave), got %d", len(calls))
	}
	for i, b := range calls {
		if len(b) != 3 {
			t.Errorf("batch %d: want 3 writes (3 rows), got %d", i, len(b))
		}
	}

	// All wave-1 writes must be for column 'a'; wave-2 for 'b'.
	for _, b := range calls[0] {
		// column index of "a" within w.Columns is 0
		if b.ColIdx != 0 {
			t.Errorf("wave 1 contained write to col_idx=%d (want 0=A)", b.ColIdx)
		}
	}
	for _, b := range calls[1] {
		if b.ColIdx != 1 {
			t.Errorf("wave 2 contained write to col_idx=%d (want 1=B)", b.ColIdx)
		}
	}
}

func TestSheetWriter_SkipsErrorCellsInBatch(t *testing.T) {
	// 1 wave, 3 rows. Row 1 fails permanently → batch has 2 writes,
	// not 3.
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "A", Prompt: "p", Type: "text"},
	})
	exec := &mockExecutor{behavior: func(t CellTask, _ int) (CellResult, error) {
		if t.RowIdx == 1 {
			return CellResult{}, errPermanent
		}
		return CellResult{Value: t.Column.ID + ":" + intStr(t.RowIdx)}, nil
	}}
	bus := &recordingBus{}
	writer := &recordingWriter{failOn: -1}
	o := New(st, exec, bus, writer)
	o.baseBackoff = 1 * time.Millisecond

	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(3, "C"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.completed", 1, 3*time.Second) {
		t.Fatal("run did not complete")
	}

	calls := writer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 batch, got %d", len(calls))
	}
	if got := len(calls[0]); got != 2 {
		t.Errorf("want 2 writes in batch (row 1 failed), got %d", got)
	}
	for _, b := range calls[0] {
		if b.RowIdx == 1 {
			t.Errorf("failed row 1 should NOT be in write batch")
		}
	}
}

func TestSheetWriter_FailureFailsRun(t *testing.T) {
	// Writer returns an error on the first call → run should be marked
	// error (sheet has diverged from orchestrator state, that's a worse
	// failure than per-cell errors).
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "A", Prompt: "p", Type: "text"},
	})
	exec := &mockExecutor{behavior: func(t CellTask, _ int) (CellResult, error) {
		return CellResult{Value: "ok"}, nil
	}}
	bus := &recordingBus{}
	writer := &recordingWriter{failOn: 0, err: errWriterDown}
	o := New(st, exec, bus, writer)

	runID, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(2, "C"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.error", 1, 2*time.Second) {
		t.Fatal("expected run.error event on writer failure")
	}

	r, _ := st.GetRun(context.Background(), runID)
	if r.Status != "error" {
		t.Errorf("run status: want error, got %s", r.Status)
	}
	if r.ErrorMessage == nil || *r.ErrorMessage == "" {
		t.Errorf("expected error_message to be populated")
	}
}

var (
	errPermanent  = newErr("permanent")
	errWriterDown = newErr("writer down")
)

func newErr(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
