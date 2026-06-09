// Package runtime is the sheet-workflows orchestrator. Given a workflow
// definition (target Google Sheet + typed column schema + DAG by
// depends_on), it fans out per-row × per-column "enrichment" tasks
// subject to a per-tenant concurrency cap, retries failed cells with
// backoff, batches updates back to Google Sheets to respect quota, and
// streams progress events to the SPA via the goclaw WS bus.
//
// Architecture:
//
//	StartRun → init cells in DB → compute DAG waves → schedule executor
//	loop → publish progress events → finalize → emit completed event.
//
// The CellExecutor interface decouples this package from the actual
// LLM call + Sheet write; PR3 wires up an implementation that goes
// through the sheets-mcp tools.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Defaults — overridable per Orchestrator instance for tests.
const (
	defaultMaxConcurrentCells = 20
	defaultMaxAttempts        = 3
	defaultBaseBackoff        = 2 * time.Second
	defaultProgressFlushEvery = 500 * time.Millisecond
)

// ─── Public types ────────────────────────────────────────────────────

// StartRunInput captures what's needed to kick off a workflow run.
// Constructed by callers (chat agent, cron service, webhook handler).
type StartRunInput struct {
	WorkflowID     uuid.UUID
	TenantID       uuid.UUID
	UserID         string // Cognito sub of the actor (for billing headers)
	TriggeredBy    string // manual | cron | webhook | retry
	TriggerPayload []byte // raw jsonb (optional)

	// When non-nil, only enrich cells whose (row, col) match the filter —
	// used by retry-failed-cells and by webhook-row-only triggers.
	CellFilter func(rowIdx, colIdx int) bool

	// Pre-fetched row context. If nil, the executor reads via CellExecutor.
	// Map: rowIdx → {colName → existing cell text}.
	Rows map[int]map[string]string

	// Optional: override default concurrency cap for this run.
	MaxConcurrent int
}

// RunEvent is what the orchestrator emits to the WS bus / SPA. Keep
// fields stable — SPA decoders depend on this shape.
type RunEvent struct {
	Type       string    `json:"type"` // run.started | cell.update | run.progress | run.completed | run.error
	RunID      uuid.UUID `json:"run_id"`
	WorkflowID uuid.UUID `json:"workflow_id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	UserID     string    `json:"user_id"`

	// cell.update fields
	RowIdx     *int    `json:"row_idx,omitempty"`
	ColIdx     *int    `json:"col_idx,omitempty"`
	CellStatus *string `json:"cell_status,omitempty"`
	CellError  *string `json:"cell_error,omitempty"`

	// run.progress / run.completed fields
	Completed int    `json:"completed,omitempty"`
	Errored   int    `json:"errored,omitempty"`
	Total     int    `json:"total,omitempty"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
}

// EventBus is the publish-only interface the orchestrator uses to push
// progress to the SPA. Implemented by goclaw's existing WS gateway in
// PR3 (we don't reach into gateway from here to keep the dep arrow
// pointing outward).
type EventBus interface {
	PublishWorkflowEvent(ctx context.Context, ev RunEvent)
}

// CellExecutor is the per-cell unit of work — looks up row context,
// builds a prompt from the column definition, calls the LLM (via the
// web-agent-api proxy with billing X-Actor-* headers), validates the
// structured output against the column type, and writes the value back
// to Google Sheets through sheets_batch_update.
//
// Returns the resolved cell value (for batching upstream), tokens
// consumed, and per-cell latency. An error aborts THIS cell only — the
// orchestrator catches, increments attempt, and re-queues with backoff
// until maxAttempts.
type CellExecutor interface {
	ExecuteCell(ctx context.Context, task CellTask) (CellResult, error)
}

// CellTask is everything ExecuteCell needs to do its job in isolation.
type CellTask struct {
	RunID         uuid.UUID
	WorkflowID    uuid.UUID
	TenantID      uuid.UUID
	UserID        string
	SpreadsheetID string
	SheetTab      string
	TargetRange   string

	RowIdx int
	ColIdx int
	Column store.SheetWorkflowColumn

	// Row context: every already-known column value for this row, keyed by
	// column ID. The executor uses it to satisfy `depends_on` (e.g. the
	// "LinkedIn" column's prompt sees "CEO=Jane Doe" in context).
	RowContext map[string]string

	Attempt int // 0-based; ExecuteCell sees current attempt number
}

type CellResult struct {
	Value     string // raw text to write to Sheet
	TokensIn  int
	TokensOut int
	LatencyMs int
}

// CellWrite is one (row, col, value) entry the orchestrator collects per
// wave and flushes through SheetWriter in one batched API call.
type CellWrite struct {
	WorkflowID    uuid.UUID
	SpreadsheetID string
	SheetTab      string
	TargetRange   string
	RowIdx        int
	ColIdx        int
	Value         string
}

// SheetWriter pushes a batch of cell writes back to the user's Google
// Sheet. Concrete impl in PR3b wraps sheets-mcp `sheets_batch_update`.
// In tests a recording writer asserts the orchestrator collected and
// flushed the right batches at the right times.
//
// Implementations should respect Google's 60/min/user write quota by
// merging contiguous cells into single ranges where possible. The
// orchestrator already buffers per-wave so callers see at most one
// batch per wave; if a wave has >50 cells, the writer may split into
// multiple calls but must atomic-flush before returning.
type SheetWriter interface {
	BatchWrite(ctx context.Context, userID string, writes []CellWrite) error
}

// ─── Orchestrator ────────────────────────────────────────────────────

// Orchestrator owns active runs, schedules cell tasks, throttles
// concurrency per tenant, retries with backoff, flushes progress to DB
// + WS bus. One instance per goclaw process; safe for concurrent
// StartRun calls.
type Orchestrator struct {
	store    store.SheetWorkflowStore
	executor CellExecutor
	bus      EventBus
	writer   SheetWriter // optional — when nil, results are tracked in DB only

	maxConcurrent int
	maxAttempts   int
	baseBackoff   time.Duration

	// Per-tenant semaphore (lazy-init). Caps concurrent CELLS in flight.
	tenantSemMu sync.Mutex
	tenantSem   map[uuid.UUID]chan struct{}

	// Active runs registry — used by Cancel + recovery scanner to find
	// in-flight work belonging to this process.
	activeMu sync.RWMutex
	active   map[uuid.UUID]context.CancelFunc
}

func New(s store.SheetWorkflowStore, ex CellExecutor, bus EventBus, writer SheetWriter) *Orchestrator {
	return &Orchestrator{
		store:         s,
		executor:      ex,
		bus:           bus,
		writer:        writer,
		maxConcurrent: defaultMaxConcurrentCells,
		maxAttempts:   defaultMaxAttempts,
		baseBackoff:   defaultBaseBackoff,
		tenantSem:     map[uuid.UUID]chan struct{}{},
		active:        map[uuid.UUID]context.CancelFunc{},
	}
}

// SetMaxConcurrent overrides the default per-tenant in-flight cells cap.
// Safe to call after New() but before the first StartRun — typically
// called by the gateway boot path from config (Workflows.MaxConcurrent).
// Values ≤0 are ignored so callers can pass through optional config.
func (o *Orchestrator) SetMaxConcurrent(n int) {
	if n > 0 {
		o.maxConcurrent = n
	}
}

// StartRun kicks off a run for the given workflow. Returns the run_id
// immediately; the actual fanout runs asynchronously. Callers (chat
// agent, webhook, cron) get progress via the EventBus.
func (o *Orchestrator) StartRun(ctx context.Context, in StartRunInput) (uuid.UUID, error) {
	w, err := o.store.GetWorkflow(ctx, in.WorkflowID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get workflow: %w", err)
	}
	if w == nil {
		return uuid.Nil, errors.New("workflow not found")
	}
	if w.Status != "active" {
		return uuid.Nil, fmt.Errorf("workflow status=%s (expected active)", w.Status)
	}

	// Determine row count. Caller may pre-supply rows; else assume 0 and
	// let the executor figure it out on first read. Simple path first:
	// require caller to provide row count via len(in.Rows).
	rowCount := len(in.Rows)
	colCount := len(w.Columns)
	if colCount == 0 {
		return uuid.Nil, errors.New("workflow has no columns")
	}
	if rowCount == 0 {
		return uuid.Nil, errors.New("no rows to enrich (executor must pre-fill StartRunInput.Rows)")
	}

	run := &store.SheetWorkflowRun{
		WorkflowID:     w.ID,
		TenantID:       w.TenantID,
		TriggeredBy:    in.TriggeredBy,
		TriggerPayload: in.TriggerPayload,
		Status:         "queued",
		RowCount:       rowCount,
	}
	if err := o.store.CreateRun(ctx, run); err != nil {
		return uuid.Nil, fmt.Errorf("create run: %w", err)
	}
	if err := o.store.BulkInitCells(ctx, run.ID, rowCount, colCount); err != nil {
		return uuid.Nil, fmt.Errorf("init cells: %w", err)
	}

	// Detach from caller context so the run survives the originating WS
	// request closing. Cancellation goes through o.Cancel(runID).
	runCtx, cancel := context.WithCancel(context.Background())
	o.activeMu.Lock()
	o.active[run.ID] = cancel
	o.activeMu.Unlock()

	o.emit(runCtx, RunEvent{
		Type:       "run.started",
		RunID:      run.ID,
		WorkflowID: w.ID,
		TenantID:   w.TenantID,
		UserID:     in.UserID,
		Total:      rowCount * colCount,
	})

	go o.executeRun(runCtx, w, run, in)

	return run.ID, nil
}

// Cancel marks a running run as cancelled. Best-effort: in-flight cells
// finish their current attempt, but no further cells are scheduled.
func (o *Orchestrator) Cancel(ctx context.Context, runID uuid.UUID) error {
	o.activeMu.Lock()
	cancel, ok := o.active[runID]
	o.activeMu.Unlock()
	if !ok {
		return errors.New("run not active on this instance")
	}
	cancel()
	return o.store.FinishRun(ctx, runID, "cancelled", strPtr("cancelled by user"))
}

// ─── Core execution ──────────────────────────────────────────────────

func (o *Orchestrator) executeRun(ctx context.Context, w *store.SheetWorkflow, run *store.SheetWorkflowRun, in StartRunInput) {
	defer func() {
		o.activeMu.Lock()
		delete(o.active, run.ID)
		o.activeMu.Unlock()
	}()

	waves, err := planDAG(w.Columns)
	if err != nil {
		o.failRun(ctx, run, w, in.UserID, fmt.Errorf("plan dag: %w", err))
		return
	}

	progress := newProgressTracker(rowCountFromInput(in), len(w.Columns))
	// Row context: shared map of resolved values, keyed by rowIdx then
	// column ID. Seeded with pre-fetched cells from input.
	rowCtx := map[int]map[string]string{}
	for rowIdx, byName := range in.Rows {
		entry := map[string]string{}
		for _, col := range w.Columns {
			if v, ok := byName[col.Name]; ok && v != "" {
				entry[col.ID] = v
			}
		}
		rowCtx[rowIdx] = entry
	}
	var rowCtxMu sync.Mutex

	maxConc := o.maxConcurrent
	if in.MaxConcurrent > 0 {
		maxConc = in.MaxConcurrent
	}

	for waveIdx, wave := range waves {
		select {
		case <-ctx.Done():
			o.failRun(ctx, run, w, in.UserID, errors.New("cancelled"))
			return
		default:
		}

		// Per-wave write buffer: orchestrator collects every successful
		// cell value here and flushes the whole batch via SheetWriter at
		// wave end. Keeps Sheet API calls to one per wave instead of one
		// per cell — critical for staying under Google's write quota.
		var writeBufMu sync.Mutex
		var writeBuf []CellWrite
		appendWrite := func(t CellTask, value string) {
			writeBufMu.Lock()
			defer writeBufMu.Unlock()
			writeBuf = append(writeBuf, CellWrite{
				WorkflowID:    w.ID,
				SpreadsheetID: w.SpreadsheetID,
				SheetTab:      w.SheetTab,
				TargetRange:   w.TargetRange,
				RowIdx:        t.RowIdx,
				ColIdx:        t.ColIdx,
				Value:         value,
			})
		}

		var wg sync.WaitGroup
		tasks := make(chan CellTask, 64)

		// Worker pool, bounded by min(maxConc, len(wave)*rowCount).
		workers := maxConc
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range tasks {
					o.runCellWithRetry(ctx, t, w, in.UserID, progress, &rowCtxMu, rowCtx, appendWrite)
				}
			}()
		}

		// Schedule (row × col) for this wave.
		for rowIdx := 0; rowIdx < progress.totalRows; rowIdx++ {
			for _, col := range wave {
				colIdx := columnIndex(w.Columns, col.ID)
				if in.CellFilter != nil && !in.CellFilter(rowIdx, colIdx) {
					continue
				}
				rowCtxMu.Lock()
				rc := copyStringMap(rowCtx[rowIdx])
				rowCtxMu.Unlock()
				tasks <- CellTask{
					RunID:         run.ID,
					WorkflowID:    w.ID,
					TenantID:      w.TenantID,
					UserID:        in.UserID,
					SpreadsheetID: w.SpreadsheetID,
					SheetTab:      w.SheetTab,
					TargetRange:   w.TargetRange,
					RowIdx:        rowIdx,
					ColIdx:        colIdx,
					Column:        col,
					RowContext:    rc,
				}
			}
		}
		close(tasks)
		wg.Wait()

		// Batch-flush this wave's cell writes back to the user's
		// Google Sheet. If writer is nil (tests / DB-only mode), skip.
		// Errors here mark the run as `error` because the user-visible
		// sheet has fallen out of sync with the orchestrator's DB
		// state — that's a worse failure than per-cell errors.
		if o.writer != nil && len(writeBuf) > 0 {
			if err := o.writer.BatchWrite(ctx, in.UserID, writeBuf); err != nil {
				o.failRun(ctx, run, w, in.UserID, fmt.Errorf("sheet write (wave %d): %w", waveIdx+1, err))
				return
			}
		}

		slog.Info("workflow wave done",
			"run_id", run.ID, "wave", waveIdx+1, "of", len(waves),
			"completed", progress.completed.Load(), "errors", progress.errored.Load(),
			"written", len(writeBuf),
		)

		// Flush progress to DB + WS bus between waves.
		o.flushProgress(ctx, run, w, in.UserID, progress)
	}

	// Run-level finalize.
	final := "done"
	if progress.errored.Load() > 0 && progress.completed.Load() == 0 {
		final = "error"
	}
	if err := o.store.FinishRun(ctx, run.ID, final, nil); err != nil {
		slog.Warn("finish run", "err", err)
	}
	o.emit(ctx, RunEvent{
		Type:       "run.completed",
		RunID:      run.ID,
		WorkflowID: w.ID,
		TenantID:   w.TenantID,
		UserID:     in.UserID,
		Status:     final,
		Completed:  int(progress.completed.Load()),
		Errored:    int(progress.errored.Load()),
		Total:      progress.totalCells(),
	})
}

func (o *Orchestrator) runCellWithRetry(
	ctx context.Context, t CellTask, w *store.SheetWorkflow, userID string,
	prog *progressTracker, rowCtxMu *sync.Mutex, rowCtx map[int]map[string]string,
	appendWrite func(CellTask, string),
) {
	sem := o.acquireTenantSlot(w.TenantID)
	defer o.releaseTenantSlot(sem)

	for t.Attempt < o.maxAttempts {
		select {
		case <-ctx.Done():
			return
		default:
		}

		o.markCellStatus(ctx, t, "running", nil, 0, 0, nil)
		o.emit(ctx, RunEvent{
			Type:       "cell.update",
			RunID:      t.RunID,
			WorkflowID: t.WorkflowID,
			TenantID:   t.TenantID,
			UserID:     userID,
			RowIdx:     intPtr(t.RowIdx),
			ColIdx:     intPtr(t.ColIdx),
			CellStatus: strPtr("running"),
		})

		start := time.Now()
		res, err := o.executor.ExecuteCell(ctx, t)
		latency := int(time.Since(start).Milliseconds())

		if err == nil {
			o.markCellStatus(ctx, t, "done", nil, res.TokensIn, res.TokensOut, &latency)
			prog.cellDone(res.TokensIn, res.TokensOut)
			// Update shared row context so dependent columns see this value.
			rowCtxMu.Lock()
			if rowCtx[t.RowIdx] == nil {
				rowCtx[t.RowIdx] = map[string]string{}
			}
			rowCtx[t.RowIdx][t.Column.ID] = res.Value
			rowCtxMu.Unlock()
			// Queue write to Google Sheet — flushed in one batch per wave.
			if appendWrite != nil {
				appendWrite(t, res.Value)
			}
			o.emit(ctx, RunEvent{
				Type:       "cell.update",
				RunID:      t.RunID,
				WorkflowID: t.WorkflowID,
				TenantID:   t.TenantID,
				UserID:     userID,
				RowIdx:     intPtr(t.RowIdx),
				ColIdx:     intPtr(t.ColIdx),
				CellStatus: strPtr("done"),
			})
			return
		}

		t.Attempt++
		errStr := err.Error()
		if t.Attempt >= o.maxAttempts {
			o.markCellStatus(ctx, t, "error", &errStr, 0, 0, &latency)
			prog.cellError()
			o.emit(ctx, RunEvent{
				Type:       "cell.update",
				RunID:      t.RunID,
				WorkflowID: t.WorkflowID,
				TenantID:   t.TenantID,
				UserID:     userID,
				RowIdx:     intPtr(t.RowIdx),
				ColIdx:     intPtr(t.ColIdx),
				CellStatus: strPtr("error"),
				CellError:  &errStr,
			})
			return
		}

		// Exponential backoff: base × 2^(attempt-1).
		wait := o.baseBackoff * (1 << (t.Attempt - 1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (o *Orchestrator) markCellStatus(ctx context.Context, t CellTask, status string, errMsg *string, tokensIn, tokensOut int, latencyMs *int) {
	if err := o.store.UpdateCellStatus(ctx, t.RunID, t.RowIdx, t.ColIdx, status, errMsg, t.Attempt, tokensIn, tokensOut, latencyMs); err != nil {
		slog.Warn("update cell", "run", t.RunID, "row", t.RowIdx, "col", t.ColIdx, "err", err)
	}
}

func (o *Orchestrator) flushProgress(ctx context.Context, run *store.SheetWorkflowRun, w *store.SheetWorkflow, userID string, prog *progressTracker) {
	completed := int(prog.completed.Load())
	errored := int(prog.errored.Load())
	tokIn := int(prog.tokensIn.Load())
	tokOut := int(prog.tokensOut.Load())
	if err := o.store.UpdateRunProgress(ctx, run.ID, completed, errored, tokIn, tokOut); err != nil {
		slog.Warn("flush progress", "run", run.ID, "err", err)
	}
	o.emit(ctx, RunEvent{
		Type:       "run.progress",
		RunID:      run.ID,
		WorkflowID: w.ID,
		TenantID:   w.TenantID,
		UserID:     userID,
		Completed:  completed,
		Errored:    errored,
		Total:      prog.totalCells(),
	})
}

func (o *Orchestrator) failRun(ctx context.Context, run *store.SheetWorkflowRun, w *store.SheetWorkflow, userID string, err error) {
	msg := err.Error()
	if e := o.store.FinishRun(ctx, run.ID, "error", &msg); e != nil {
		slog.Warn("fail run", "err", e)
	}
	o.emit(ctx, RunEvent{
		Type:       "run.error",
		RunID:      run.ID,
		WorkflowID: w.ID,
		TenantID:   w.TenantID,
		UserID:     userID,
		Status:     "error",
		Message:    msg,
	})
}

func (o *Orchestrator) emit(ctx context.Context, ev RunEvent) {
	if o.bus == nil {
		return
	}
	o.bus.PublishWorkflowEvent(ctx, ev)
}

// ─── Per-tenant concurrency cap ──────────────────────────────────────

func (o *Orchestrator) acquireTenantSlot(tenantID uuid.UUID) chan struct{} {
	o.tenantSemMu.Lock()
	sem, ok := o.tenantSem[tenantID]
	if !ok {
		sem = make(chan struct{}, o.maxConcurrent)
		o.tenantSem[tenantID] = sem
	}
	o.tenantSemMu.Unlock()
	sem <- struct{}{}
	return sem
}

func (o *Orchestrator) releaseTenantSlot(sem chan struct{}) {
	<-sem
}

// ─── Helpers ────────────────────────────────────────────────────────

func rowCountFromInput(in StartRunInput) int {
	max := 0
	for k := range in.Rows {
		if k+1 > max {
			max = k + 1
		}
	}
	return max
}

func columnIndex(cols []store.SheetWorkflowColumn, id string) int {
	for i, c := range cols {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
