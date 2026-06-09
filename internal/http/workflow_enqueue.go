package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/workflow/runtime"
)

// WorkflowEnqueueHandler exposes POST /v1/internal/workflows/enqueue —
// an instance-internal endpoint for trusted callers (the
// sheets_enrich_run MCP tool, cron service, webhook router) to kick
// off a workflow run.
//
// Auth: gateway bearer token (same pattern as MediaImportHandler).
//
// Body shape:
//
//	{
//	  "workflow_id": "<uuid, optional>",  // when omitted, create ephemeral workflow from inline fields
//	  "tenant_id":   "<uuid, optional>",  // when omitted, derived from user_id via tenant_users
//	  "user_id":     "<cognito sub>",
//	  "spreadsheet_id": "...", "sheet_tab": "Sheet1", "target_range": "A2:Z",
//	  "columns": [{id, name, prompt, type, depends_on:[...]}, ...],
//	  "rows":    {"0": {"company": "Acme"}, "1": {"company": "Beta"}},
//	  "triggered_by": "manual" | "cron" | "webhook" | "retry",
//	  "trigger_payload": <raw json, optional>,
//	  "max_concurrent": 20
//	}
//
// Returns:
//
//	202 {"run_id": "...", "status": "queued"}
//
// Validation errors map to 400; auth to 401; storage / orchestrator
// errors to 5xx so trusted callers can retry idempotently.
type WorkflowEnqueueHandler struct {
	workflowStore store.SheetWorkflowStore
	orchestrator  *runtime.Orchestrator
	// resolveTenant maps a user_id to its tenant_id. Used when the
	// caller omits tenant_id from the request body — MCP tools like
	// sheets_enrich_run usually only know the user, and the tenant
	// can be looked up from tenant_users(user_id → tenant_id).
	// nil → tenant_id must be supplied in the request body.
	resolveTenant func(ctx context.Context, userID string) (uuid.UUID, error)
}

// NewWorkflowEnqueueHandler constructs the handler. resolveTenant is
// optional; when nil, callers must supply tenant_id in the request
// body. In production wiring this is a closure over the goclaw users
// store; passing nil is fine for tests.
func NewWorkflowEnqueueHandler(s store.SheetWorkflowStore, o *runtime.Orchestrator) *WorkflowEnqueueHandler {
	return &WorkflowEnqueueHandler{workflowStore: s, orchestrator: o}
}

// WithTenantResolver returns h with the tenant resolver attached.
// Wiring uses this so the inline-construction call site doesn't grow
// extra positional args.
func (h *WorkflowEnqueueHandler) WithTenantResolver(fn func(ctx context.Context, userID string) (uuid.UUID, error)) *WorkflowEnqueueHandler {
	h.resolveTenant = fn
	return h
}

func (h *WorkflowEnqueueHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/internal/workflows/enqueue", h.auth(h.handleEnqueue))
}

func (h *WorkflowEnqueueHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pkgGatewayToken == "" {
			http.Error(w, "internal endpoint disabled (no gateway token configured)", http.StatusServiceUnavailable)
			return
		}
		if !tokenMatch(extractBearerToken(r), pkgGatewayToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// EnqueueRequest is exported so the corresponding MCP tool / cron
// caller / webhook router can build it with strong typing instead of
// hand-rolling JSON.
type EnqueueRequest struct {
	WorkflowID *uuid.UUID `json:"workflow_id,omitempty"`

	TenantID uuid.UUID `json:"tenant_id"`
	UserID   string    `json:"user_id"`

	// Inline definition — used when WorkflowID is nil. Otherwise the
	// stored workflow's schema wins and these fields are ignored.
	Name          string                       `json:"name,omitempty"`
	SpreadsheetID string                       `json:"spreadsheet_id,omitempty"`
	SheetTab      string                       `json:"sheet_tab,omitempty"`
	TargetRange   string                       `json:"target_range,omitempty"`
	Columns       []store.SheetWorkflowColumn  `json:"columns,omitempty"`

	Rows           map[string]map[string]string `json:"rows"`
	TriggeredBy    string                       `json:"triggered_by"`
	TriggerPayload json.RawMessage              `json:"trigger_payload,omitempty"`
	MaxConcurrent  int                          `json:"max_concurrent,omitempty"`
}

// EnqueueResponse mirrors the orchestrator's StartRun return shape.
type EnqueueResponse struct {
	RunID  uuid.UUID `json:"run_id"`
	Status string    `json:"status"`
}

// EnqueueAsUser is the core enqueue logic re-usable by trusted server-
// side callers that have already authenticated their tenant + user
// identity through some other path (e.g. the WS workflow.enqueue RPC,
// which reads tenant + user from the gateway client session).
//
// HTTP callers go through handleEnqueue which still does its own bearer
// auth + body decode; this method is the bottom half. Returns the run
// id on success or an error suitable for surfacing to the client.
func (h *WorkflowEnqueueHandler) EnqueueAsUser(ctx context.Context, tenantID uuid.UUID, userID string, req *EnqueueRequest) (uuid.UUID, error) {
	if h.orchestrator == nil || h.workflowStore == nil {
		return uuid.Nil, errors.New("workflow runtime not configured")
	}
	if tenantID == uuid.Nil {
		return uuid.Nil, errors.New("tenant_id required")
	}
	if userID == "" {
		return uuid.Nil, errors.New("user_id required")
	}
	// Caller's identity is authoritative — never trust body fields.
	req.TenantID = tenantID
	req.UserID = userID

	if err := validateEnqueue(req); err != nil {
		return uuid.Nil, err
	}

	wfID, err := resolveOrCreateWorkflow(ctx, h.workflowStore, req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("workflow store: %w", err)
	}

	rowsByIdx := make(map[int]map[string]string, len(req.Rows))
	for k, v := range req.Rows {
		idx, perr := parseRowIdx(k)
		if perr != nil {
			return uuid.Nil, fmt.Errorf("invalid row index %q", k)
		}
		rowsByIdx[idx] = v
	}

	runID, err := h.orchestrator.StartRun(ctx, runtime.StartRunInput{
		WorkflowID:     wfID,
		TenantID:       req.TenantID,
		UserID:         req.UserID,
		TriggeredBy:    req.TriggeredBy,
		TriggerPayload: []byte(req.TriggerPayload),
		Rows:           rowsByIdx,
		MaxConcurrent:  req.MaxConcurrent,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("start run: %w", err)
	}
	return runID, nil
}

func (h *WorkflowEnqueueHandler) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if h.orchestrator == nil || h.workflowStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "workflow runtime not configured"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<20) // 4 MiB cap

	var req EnqueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}

	if err := validateEnqueue(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Derive tenant_id from user_id when the caller omitted it.
	// sheets_enrich_run (the most common caller) is an MCP tool that
	// knows the cognito sub but doesn't have direct access to the
	// tenant — goclaw can look it up via tenant_users.
	if req.TenantID == uuid.Nil {
		if h.resolveTenant == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id missing and no resolver configured"})
			return
		}
		tid, err := h.resolveTenant(ctx, req.UserID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resolve tenant from user: " + err.Error()})
			return
		}
		req.TenantID = tid
	}

	// Resolve / create workflow.
	wfID, err := resolveOrCreateWorkflow(ctx, h.workflowStore, &req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workflow store: " + err.Error()})
		return
	}

	// Convert string keys ("0", "1", ...) to ints. Map shape is JSON-y
	// because Go's map[int]X doesn't survive json.Unmarshal naturally.
	rowsByIdx := make(map[int]map[string]string, len(req.Rows))
	for k, v := range req.Rows {
		idx, perr := parseRowIdx(k)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid row index " + k})
			return
		}
		rowsByIdx[idx] = v
	}

	runID, err := h.orchestrator.StartRun(ctx, runtime.StartRunInput{
		WorkflowID:     wfID,
		TenantID:       req.TenantID,
		UserID:         req.UserID,
		TriggeredBy:    req.TriggeredBy,
		TriggerPayload: []byte(req.TriggerPayload),
		Rows:           rowsByIdx,
		MaxConcurrent:  req.MaxConcurrent,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "start run: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusAccepted, EnqueueResponse{RunID: runID, Status: "queued"})
}

func validateEnqueue(req *EnqueueRequest) error {
	// tenant_id is optional — derived from user_id later if absent.
	// user_id is the source of truth for tenant resolution, so its
	// presence is enforced unconditionally.
	if strings.TrimSpace(req.UserID) == "" {
		return errors.New("user_id is required")
	}
	if req.TriggeredBy == "" {
		req.TriggeredBy = "manual"
	}
	switch req.TriggeredBy {
	case "manual", "cron", "webhook", "retry":
	default:
		return errors.New("triggered_by must be one of: manual, cron, webhook, retry")
	}
	if len(req.Rows) == 0 {
		return errors.New("rows must not be empty")
	}
	if req.WorkflowID == nil {
		// Inline def — require minimal fields.
		if strings.TrimSpace(req.SpreadsheetID) == "" {
			return errors.New("spreadsheet_id is required when workflow_id is omitted")
		}
		if len(req.Columns) == 0 {
			return errors.New("columns is required when workflow_id is omitted")
		}
	}
	return nil
}

func resolveOrCreateWorkflow(ctx context.Context, s store.SheetWorkflowStore, req *EnqueueRequest) (uuid.UUID, error) {
	if req.WorkflowID != nil {
		w, err := s.GetWorkflow(ctx, *req.WorkflowID)
		if err != nil {
			return uuid.Nil, err
		}
		if w == nil {
			return uuid.Nil, errors.New("workflow not found")
		}
		if w.Status != "active" {
			return uuid.Nil, errors.New("workflow status=" + w.Status + " (must be active)")
		}
		return w.ID, nil
	}
	// Ephemeral: create a workflow record on the fly. Auto-cleanup
	// vacuum picks these up after 30 days idle (see migration 000073).
	w := &store.SheetWorkflow{
		TenantID:      req.TenantID,
		UserID:        req.UserID,
		Name:          nameOrDefault(req.Name, "ad-hoc enrichment"),
		SpreadsheetID: req.SpreadsheetID,
		SheetTab:      defaultStr(req.SheetTab, "Sheet1"),
		TargetRange:   defaultStr(req.TargetRange, "A2:Z"),
		Columns:       req.Columns,
		Visibility:    "personal",
		Status:        "active",
	}
	if err := s.CreateWorkflow(ctx, w); err != nil {
		return uuid.Nil, err
	}
	return w.ID, nil
}

func parseRowIdx(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("non-digit")
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0, errors.New("too large")
		}
	}
	return n, nil
}

func nameOrDefault(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return s
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
