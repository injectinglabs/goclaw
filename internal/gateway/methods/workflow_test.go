package methods

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/memory"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// seedRun is a tiny helper that bootstraps a workflow + run + cells
// in the memory store so each test reads cleanly. Returns runID + the
// chosen tenantID so callers can wire NewTestClientWithSend.
func seedRun(t *testing.T, s *memory.SheetWorkflowStore, status string, rowCount, colCount int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenantID := uuid.New()
	workflowID := uuid.New()
	runID := uuid.New()

	if err := s.CreateWorkflow(ctx, &store.SheetWorkflow{
		ID:       workflowID,
		TenantID: tenantID,
		UserID:   "user-1",
		Name:     "Test workflow",
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	if err := s.CreateRun(ctx, &store.SheetWorkflowRun{
		ID:             runID,
		WorkflowID:     workflowID,
		TenantID:       tenantID,
		Status:         status,
		RowCount:       rowCount,
		CompletedCount: 0,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.BulkInitCells(ctx, runID, rowCount, colCount); err != nil {
		t.Fatalf("BulkInitCells: %v", err)
	}
	return runID, tenantID
}

func runStateReqFrame(t *testing.T, runID string) *protocol.RequestFrame {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"run_id": runID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &protocol.RequestFrame{
		Type:   protocol.FrameTypeRequest,
		ID:     "req-1",
		Method: protocol.MethodWorkflowRunState,
		Params: raw,
	}
}

func runStateReadResponse(t *testing.T, ch <-chan []byte) *protocol.ResponseFrame {
	t.Helper()
	select {
	case data := <-ch:
		var resp protocol.ResponseFrame
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("unmarshal response: %v\nraw: %s", err, data)
		}
		return &resp
	case <-time.After(2 * time.Second):
		t.Fatal("no response within 2s")
		return nil
	}
}

// ─── Happy path ─────────────────────────────────────────────────────

// TestWorkflowRunState_HappyPath_ReturnsRunAndCells covers the
// canonical case: same-tenant caller queries an existing run, gets
// the run header + the full cell grid in (row,col) order.
func TestWorkflowRunState_HappyPath_ReturnsRunAndCells(t *testing.T) {
	s := memory.NewSheetWorkflowStore()
	runID, tenantID := seedRun(t, s, "running", 2, 3) // 2×3 = 6 cells

	m := NewWorkflowMethods(s)
	client, sendCh := gateway.NewTestClientWithSend(permissions.RoleViewer, tenantID, "user-1")

	m.handleRunState(context.Background(), client, runStateReqFrame(t, runID.String()))

	resp := runStateReadResponse(t, sendCh)
	if resp.Error != nil {
		t.Fatalf("expected OK, got error: %+v", resp.Error)
	}

	// ResponseFrame.Payload is `any` — after round-trip it's a map.
	// Re-marshal then unmarshal into the typed shape we want to assert on.
	raw, err := json.Marshal(resp.Payload)
	if err != nil {
		t.Fatalf("re-marshal payload: %v", err)
	}
	var payload struct {
		Run   runStateRun    `json:"run"`
		Cells []runStateCell `json:"cells"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if payload.Run.ID != runID {
		t.Errorf("run.id: got %s want %s", payload.Run.ID, runID)
	}
	if payload.Run.Status != "running" {
		t.Errorf("run.status: got %q want %q", payload.Run.Status, "running")
	}
	if payload.Run.RowCount != 2 {
		t.Errorf("run.row_count: got %d want 2", payload.Run.RowCount)
	}
	if len(payload.Cells) != 6 {
		t.Fatalf("cells len: got %d want 6", len(payload.Cells))
	}
	// Spot-check first + last for deterministic order.
	if payload.Cells[0].RowIdx != 0 || payload.Cells[0].ColIdx != 0 {
		t.Errorf("first cell: got (%d,%d) want (0,0)", payload.Cells[0].RowIdx, payload.Cells[0].ColIdx)
	}
	last := payload.Cells[len(payload.Cells)-1]
	if last.RowIdx != 1 || last.ColIdx != 2 {
		t.Errorf("last cell: got (%d,%d) want (1,2)", last.RowIdx, last.ColIdx)
	}
}

// ─── Cross-tenant denial ────────────────────────────────────────────

// TestWorkflowRunState_CrossTenant_ReturnsNotFound asserts the
// security-critical guard: a caller from a foreign tenant gets
// NOT_FOUND (not 403), and cells are never read. The collapse to
// NOT_FOUND deliberately hides the existence of cross-tenant runs.
func TestWorkflowRunState_CrossTenant_ReturnsNotFound(t *testing.T) {
	s := memory.NewSheetWorkflowStore()
	runID, _ := seedRun(t, s, "running", 1, 1)

	m := NewWorkflowMethods(s)
	foreignTenant := uuid.New() // NOT the run's tenant
	client, sendCh := gateway.NewTestClientWithSend(permissions.RoleViewer, foreignTenant, "stranger")

	m.handleRunState(context.Background(), client, runStateReqFrame(t, runID.String()))

	resp := runStateReadResponse(t, sendCh)
	if resp.Error == nil {
		t.Fatalf("expected NOT_FOUND error, got OK with payload: %+v", resp.Payload)
	}
	if resp.Error.Code != protocol.ErrNotFound {
		t.Errorf("error code: got %q want %q", resp.Error.Code, protocol.ErrNotFound)
	}
}

// ─── Missing / malformed params ─────────────────────────────────────

func TestWorkflowRunState_MissingRunID_ReturnsInvalidRequest(t *testing.T) {
	s := memory.NewSheetWorkflowStore()
	m := NewWorkflowMethods(s)
	client, sendCh := gateway.NewTestClientWithSend(permissions.RoleViewer, uuid.New(), "user-1")

	raw, _ := json.Marshal(map[string]string{})
	req := &protocol.RequestFrame{
		Type:   protocol.FrameTypeRequest,
		ID:     "req-1",
		Method: protocol.MethodWorkflowRunState,
		Params: raw,
	}
	m.handleRunState(context.Background(), client, req)

	resp := runStateReadResponse(t, sendCh)
	if resp.Error == nil || resp.Error.Code != protocol.ErrInvalidRequest {
		t.Fatalf("expected INVALID_REQUEST, got %+v", resp.Error)
	}
}

func TestWorkflowRunState_InvalidUUID_ReturnsInvalidRequest(t *testing.T) {
	s := memory.NewSheetWorkflowStore()
	m := NewWorkflowMethods(s)
	client, sendCh := gateway.NewTestClientWithSend(permissions.RoleViewer, uuid.New(), "user-1")

	m.handleRunState(context.Background(), client, runStateReqFrame(t, "not-a-uuid"))

	resp := runStateReadResponse(t, sendCh)
	if resp.Error == nil || resp.Error.Code != protocol.ErrInvalidRequest {
		t.Fatalf("expected INVALID_REQUEST, got %+v", resp.Error)
	}
}

// ─── Unknown run ────────────────────────────────────────────────────

func TestWorkflowRunState_UnknownRun_ReturnsNotFound(t *testing.T) {
	s := memory.NewSheetWorkflowStore()
	m := NewWorkflowMethods(s)
	client, sendCh := gateway.NewTestClientWithSend(permissions.RoleViewer, uuid.New(), "user-1")

	m.handleRunState(context.Background(), client, runStateReqFrame(t, uuid.New().String()))

	resp := runStateReadResponse(t, sendCh)
	if resp.Error == nil || resp.Error.Code != protocol.ErrNotFound {
		t.Fatalf("expected NOT_FOUND, got %+v", resp.Error)
	}
}
