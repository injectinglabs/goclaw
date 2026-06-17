package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	memstore "github.com/nextlevelbuilder/goclaw/internal/store/memory"
	"github.com/nextlevelbuilder/goclaw/internal/workflow/runtime"
)

// fakeExecutor returns deterministic values so the orchestrator's
// async loop finishes quickly during the HTTP handler tests.
type fakeExecutor struct{}

func (fakeExecutor) ExecuteCell(_ context.Context, t runtime.CellTask) (runtime.CellResult, error) {
	return runtime.CellResult{Value: t.Column.Name + ":ok"}, nil
}

// silentBus / nilWriter — handler-level tests don't assert WS events
// or sheet writes; the orchestrator's own tests cover those paths.
type silentBus struct{}

func (silentBus) PublishWorkflowEvent(context.Context, runtime.RunEvent) {}

func newTestHandler(t *testing.T) (*WorkflowEnqueueHandler, *memstore.SheetWorkflowStore) {
	t.Helper()
	st := memstore.NewSheetWorkflowStore()
	o := runtime.New(st, fakeExecutor{}, silentBus{}, nil)

	// Gateway token must be set or the auth middleware returns 503.
	old := pkgGatewayToken
	pkgGatewayToken = "test-token"
	t.Cleanup(func() { pkgGatewayToken = old })

	return NewWorkflowEnqueueHandler(st, o), st
}

func enqueueBody(t *testing.T, mods ...func(*EnqueueRequest)) []byte {
	t.Helper()
	req := EnqueueRequest{
		TenantID:      uuid.New(),
		UserID:        "u-1",
		SpreadsheetID: "ss-1",
		SheetTab:      "Sheet1",
		TargetRange:   "A2:Z",
		Columns: []store.SheetWorkflowColumn{
			{ID: "a", Name: "A", Prompt: "p", Type: "text"},
		},
		Rows:        map[string]map[string]string{"0": {"company": "Acme"}},
		TriggeredBy: "manual",
	}
	for _, m := range mods {
		m(&req)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func do(t *testing.T, h *WorkflowEnqueueHandler, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	r := httptest.NewRequest("POST", "/v1/internal/workflows/enqueue", bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestEnqueue_HappyPath(t *testing.T) {
	h, st := newTestHandler(t)
	w := do(t, h, enqueueBody(t), map[string]string{
		"Authorization": "Bearer test-token",
		"Content-Type":  "application/json",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d. body=%s", w.Code, w.Body.String())
	}
	var resp EnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RunID == uuid.Nil {
		t.Errorf("run_id should be populated")
	}
	if resp.Status != "queued" {
		t.Errorf("status: want queued, got %s", resp.Status)
	}
	// Workflow was created on the fly (no workflow_id supplied) — verify.
	if st.Snapshot() == "" || !strings.Contains(st.Snapshot(), "\"workflows\":1") {
		t.Errorf("expected 1 workflow created, snapshot=%s", st.Snapshot())
	}
}

func TestEnqueue_RejectsUnauthorized(t *testing.T) {
	h, _ := newTestHandler(t)

	// Wrong token
	w := do(t, h, enqueueBody(t), map[string]string{
		"Authorization": "Bearer wrong",
		"Content-Type":  "application/json",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: want 401, got %d", w.Code)
	}

	// Missing header
	w2 := do(t, h, enqueueBody(t), map[string]string{
		"Content-Type": "application/json",
	})
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("missing header: want 401, got %d", w2.Code)
	}
}

func TestEnqueue_ValidatesBody(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []struct {
		name string
		mod  func(*EnqueueRequest)
		want string // substring of error
	}{
		{"missing tenant", func(r *EnqueueRequest) { r.TenantID = uuid.Nil }, "tenant_id"},
		{"missing user", func(r *EnqueueRequest) { r.UserID = "" }, "user_id"},
		{"empty rows", func(r *EnqueueRequest) { r.Rows = map[string]map[string]string{} }, "rows"},
		{"no spreadsheet (inline mode)", func(r *EnqueueRequest) { r.SpreadsheetID = "" }, "spreadsheet_id"},
		{"no columns (inline mode)", func(r *EnqueueRequest) { r.Columns = nil }, "columns"},
		{"bad triggered_by", func(r *EnqueueRequest) { r.TriggeredBy = "evil" }, "triggered_by"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h, enqueueBody(t, c.mod), map[string]string{
				"Authorization": "Bearer test-token",
				"Content-Type":  "application/json",
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status: want 400, got %d. body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(strings.ToLower(w.Body.String()), c.want) {
				t.Errorf("expected error mentioning %q, body=%s", c.want, w.Body.String())
			}
		})
	}
}

func TestEnqueue_AcceptsExistingWorkflow(t *testing.T) {
	h, st := newTestHandler(t)
	// Pre-create a workflow.
	pre := &store.SheetWorkflow{
		TenantID:      uuid.New(),
		UserID:        "u-1",
		Name:          "preset",
		SpreadsheetID: "ss-pre",
		SheetTab:      "Sheet1",
		TargetRange:   "A2:Z",
		Columns: []store.SheetWorkflowColumn{
			{ID: "x", Name: "X", Prompt: "p", Type: "text"},
		},
		Status: "active",
	}
	if err := st.CreateWorkflow(context.Background(), pre); err != nil {
		t.Fatal(err)
	}
	wfID := pre.ID

	w := do(t, h, enqueueBody(t, func(r *EnqueueRequest) {
		r.WorkflowID = &wfID
		r.TenantID = pre.TenantID
		// Inline fields should be ignored — clear them.
		r.SpreadsheetID = ""
		r.Columns = nil
	}), map[string]string{
		"Authorization": "Bearer test-token",
		"Content-Type":  "application/json",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d. body=%s", w.Code, w.Body.String())
	}
	// No new workflow created — should still be 1.
	if !strings.Contains(st.Snapshot(), "\"workflows\":1") {
		t.Errorf("expected workflow reused, snapshot=%s", st.Snapshot())
	}
}

func TestEnqueue_RejectsNonActiveWorkflow(t *testing.T) {
	h, st := newTestHandler(t)
	pre := &store.SheetWorkflow{
		TenantID:      uuid.New(),
		UserID:        "u-1",
		SpreadsheetID: "ss",
		Columns: []store.SheetWorkflowColumn{
			{ID: "a", Name: "A", Type: "text"},
		},
		Status: "paused",
	}
	_ = st.CreateWorkflow(context.Background(), pre)
	wfID := pre.ID
	w := do(t, h, enqueueBody(t, func(r *EnqueueRequest) {
		r.WorkflowID = &wfID
		r.TenantID = pre.TenantID
	}), map[string]string{
		"Authorization": "Bearer test-token",
		"Content-Type":  "application/json",
	})
	if w.Code != http.StatusInternalServerError {
		// 500 because validation happens after parsing — the workflow
		// lookup failure surfaces as a store error.
		t.Errorf("status: want 5xx (paused workflow), got %d. body=%s", w.Code, w.Body.String())
	}
}

func TestEnqueue_RejectsBadRowIndex(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, enqueueBody(t, func(r *EnqueueRequest) {
		r.Rows = map[string]map[string]string{"abc": {"company": "Acme"}}
	}), map[string]string{
		"Authorization": "Bearer test-token",
		"Content-Type":  "application/json",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d. body=%s", w.Code, w.Body.String())
	}
}

func TestEnqueue_RejectsMalformedJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	r := httptest.NewRequest("POST", "/v1/internal/workflows/enqueue", strings.NewReader("not-json"))
	r.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}
