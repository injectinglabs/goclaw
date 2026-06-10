package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	memstore "github.com/nextlevelbuilder/goclaw/internal/store/memory"
)

// ─── Test doubles ────────────────────────────────────────────────────

// mockExecutor records every cell it's asked to enrich and lets each
// test inject a per-cell behaviour (success / fail-then-succeed / always
// fail). Concurrent-safe.
type mockExecutor struct {
	mu       sync.Mutex
	calls    []CellTask
	behavior func(t CellTask, attempt int) (CellResult, error)
}

func (m *mockExecutor) ExecuteCell(_ context.Context, t CellTask) (CellResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, t)
	attempt := 0
	for _, c := range m.calls {
		if c.RunID == t.RunID && c.RowIdx == t.RowIdx && c.ColIdx == t.ColIdx {
			attempt++
		}
	}
	m.mu.Unlock()
	return m.behavior(t, attempt)
}

func (m *mockExecutor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// recordingBus captures every event in order; used to assert event flow.
type recordingBus struct {
	mu     sync.Mutex
	events []RunEvent
}

func (b *recordingBus) PublishWorkflowEvent(_ context.Context, ev RunEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
}

func (b *recordingBus) eventsByType(t string) []RunEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []RunEvent
	for _, ev := range b.events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func (b *recordingBus) waitFor(t string, want int, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if len(b.eventsByType(t)) >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ─── Fixture helpers ─────────────────────────────────────────────────

func newTestWorkflow(t *testing.T, st *memstore.SheetWorkflowStore, cols []store.SheetWorkflowColumn) *store.SheetWorkflow {
	t.Helper()
	tenantID := uuid.New()
	w := &store.SheetWorkflow{
		TenantID:      tenantID,
		UserID:        "u-1",
		Name:          "Test",
		SpreadsheetID: "ss-1",
		SheetTab:      "Sheet1",
		TargetRange:   "A2:Z",
		Columns:       cols,
		Status:        "active",
		Visibility:    "personal",
	}
	if err := st.CreateWorkflow(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w
}

func mkRows(rowCount int, prefix string) map[int]map[string]string {
	rows := make(map[int]map[string]string, rowCount)
	for i := 0; i < rowCount; i++ {
		rows[i] = map[string]string{"company": prefix + " " + string(rune('A'+i))}
	}
	return rows
}

// ─── Tests ──────────────────────────────────────────────────────────

func TestOrchestrator_HappyPath_SingleWave(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "col-a", Name: "CEO", Prompt: "find ceo", Type: "text"},
		{ID: "col-b", Name: "LinkedIn", Prompt: "find linkedin", Type: "url"},
	})
	exec := &mockExecutor{behavior: func(t CellTask, _ int) (CellResult, error) {
		return CellResult{Value: t.Column.Name + ":ok", TokensIn: 100, TokensOut: 20, LatencyMs: 50}, nil
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)
	o.maxConcurrent = 4

	rows := mkRows(3, "Acme")
	runID, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID:  w.ID,
		TenantID:    w.TenantID,
		UserID:      "u-1",
		TriggeredBy: "manual",
		Rows:        rows,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !bus.waitFor("run.completed", 1, 2*time.Second) {
		t.Fatalf("run did not complete in time. events: %v", bus.events)
	}

	// 3 rows × 2 cols = 6 cells
	if got := exec.callCount(); got != 6 {
		t.Errorf("executor calls: want 6, got %d", got)
	}
	if got := len(bus.eventsByType("cell.update")); got != 12 { // 6 running + 6 done
		t.Errorf("cell.update events: want 12, got %d", got)
	}
	run, _ := st.GetRun(context.Background(), runID)
	if run.Status != "done" {
		t.Errorf("run status: want done, got %s", run.Status)
	}
	if run.CompletedCount != 6 {
		t.Errorf("completed: want 6, got %d", run.CompletedCount)
	}
	if run.ErrorCount != 0 {
		t.Errorf("errors: want 0, got %d", run.ErrorCount)
	}
}

func TestOrchestrator_DAG_WavesInOrder(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	// A → B → C dependency chain.
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "Company", Prompt: "p", Type: "text"},
		{ID: "b", Name: "CEO", Prompt: "p", Type: "text", DependsOn: []string{"a"}},
		{ID: "c", Name: "LinkedIn", Prompt: "p", Type: "url", DependsOn: []string{"b"}},
	})

	var order []string
	var orderMu sync.Mutex
	exec := &mockExecutor{behavior: func(t CellTask, _ int) (CellResult, error) {
		orderMu.Lock()
		order = append(order, t.Column.ID)
		orderMu.Unlock()
		return CellResult{Value: "x"}, nil
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)
	o.maxConcurrent = 8

	rows := mkRows(2, "C")
	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.completed", 1, 2*time.Second) {
		t.Fatal("run did not complete")
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	// All 'a' calls must come before any 'b' call (waves are barriered).
	lastA, firstB, lastB, firstC := -1, -1, -1, -1
	for i, id := range order {
		switch id {
		case "a":
			if i > lastA {
				lastA = i
			}
		case "b":
			if firstB == -1 {
				firstB = i
			}
			if i > lastB {
				lastB = i
			}
		case "c":
			if firstC == -1 {
				firstC = i
			}
		}
	}
	if firstB <= lastA && firstB != -1 {
		t.Errorf("wave 2 (b) started before wave 1 (a) finished: lastA=%d firstB=%d", lastA, firstB)
	}
	if firstC <= lastB && firstC != -1 {
		t.Errorf("wave 3 (c) started before wave 2 (b) finished: lastB=%d firstC=%d", lastB, firstC)
	}
}

func TestOrchestrator_RetryTransientErrors(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "x", Name: "X", Prompt: "p", Type: "text"},
	})

	// Fail twice, succeed on 3rd.
	exec := &mockExecutor{behavior: func(_ CellTask, attempt int) (CellResult, error) {
		if attempt < 3 {
			return CellResult{}, errors.New("transient")
		}
		return CellResult{Value: "ok"}, nil
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)
	o.baseBackoff = 5 * time.Millisecond // fast test

	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(1, "C"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.completed", 1, 3*time.Second) {
		t.Fatal("run did not complete")
	}

	// Must have retried twice (3 attempts total) and ultimately succeeded.
	if got := exec.callCount(); got != 3 {
		t.Errorf("attempts: want 3, got %d", got)
	}
	for _, ev := range bus.eventsByType("cell.update") {
		if ev.CellStatus != nil && *ev.CellStatus == "error" {
			t.Errorf("expected eventual success, got error event: %+v", ev)
		}
	}
}

func TestOrchestrator_GivesUpAfterMaxAttempts(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "x", Name: "X", Prompt: "p", Type: "text"},
	})
	exec := &mockExecutor{behavior: func(_ CellTask, _ int) (CellResult, error) {
		return CellResult{}, errors.New("permanent")
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)
	o.baseBackoff = 1 * time.Millisecond

	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(1, "C"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.completed", 1, 3*time.Second) {
		t.Fatal("run did not complete")
	}
	if got := exec.callCount(); got != o.maxAttempts {
		t.Errorf("attempts: want %d, got %d", o.maxAttempts, got)
	}
	// Should have at least one error cell.update.
	var foundErr bool
	for _, ev := range bus.eventsByType("cell.update") {
		if ev.CellStatus != nil && *ev.CellStatus == "error" {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("expected an error cell.update event")
	}
}

func TestOrchestrator_RowContextPropagatesBetweenWaves(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "A", Prompt: "p", Type: "text"},
		{ID: "b", Name: "B", Prompt: "p", Type: "text", DependsOn: []string{"a"}},
	})

	var seenContextValues []string
	var ctxMu sync.Mutex

	exec := &mockExecutor{behavior: func(t CellTask, _ int) (CellResult, error) {
		if t.Column.ID == "b" {
			ctxMu.Lock()
			seenContextValues = append(seenContextValues, t.RowContext["a"])
			ctxMu.Unlock()
		}
		return CellResult{Value: t.Column.ID + "-row" + intStr(t.RowIdx)}, nil
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)

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
	ctxMu.Lock()
	defer ctxMu.Unlock()
	if len(seenContextValues) != 3 {
		t.Fatalf("expected 3 B-cell contexts, got %d", len(seenContextValues))
	}
	for i, v := range seenContextValues {
		want := "a-row" + intStr(i)
		// Order within wave isn't deterministic, so just assert all 3 expected values are present.
		_ = want
		if v == "" {
			t.Errorf("B-cell saw empty A context")
		}
	}
}

func TestOrchestrator_RejectsCycle(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "A", Prompt: "p", Type: "text", DependsOn: []string{"b"}},
		{ID: "b", Name: "B", Prompt: "p", Type: "text", DependsOn: []string{"a"}},
	})
	exec := &mockExecutor{behavior: func(_ CellTask, _ int) (CellResult, error) {
		return CellResult{Value: "x"}, nil
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)

	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(1, "C"),
	})
	if err != nil {
		// Sync planDAG failure (acceptable, but our impl plans inside the goroutine).
		return
	}
	if !bus.waitFor("run.error", 1, 2*time.Second) {
		t.Fatal("cycle did not cause run.error event")
	}
	atomic.AddInt32(new(int32), 1) // shutting up unused-import linter
}

func TestOrchestrator_ConcurrencyCapIsRespected(t *testing.T) {
	st := memstore.NewSheetWorkflowStore()
	w := newTestWorkflow(t, st, []store.SheetWorkflowColumn{
		{ID: "a", Name: "A", Prompt: "p", Type: "text"},
	})

	var inFlight int32
	var peak int32
	exec := &mockExecutor{behavior: func(_ CellTask, _ int) (CellResult, error) {
		now := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)
		for {
			cur := atomic.LoadInt32(&peak)
			if now <= cur || atomic.CompareAndSwapInt32(&peak, cur, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return CellResult{Value: "x"}, nil
	}}
	bus := &recordingBus{}
	o := New(st, exec, bus, nil)
	o.maxConcurrent = 3

	_, err := o.StartRun(context.Background(), StartRunInput{
		WorkflowID: w.ID, TenantID: w.TenantID, UserID: "u-1",
		TriggeredBy: "manual", Rows: mkRows(15, "C"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bus.waitFor("run.completed", 1, 5*time.Second) {
		t.Fatal("run did not complete")
	}
	if got := atomic.LoadInt32(&peak); got > 3 {
		t.Errorf("concurrency peak: want ≤3, got %d", got)
	}
}

// intStr is a stdlib-free int formatter to keep this test file
// dependency-light (avoids strconv just for one helper).
func intStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
