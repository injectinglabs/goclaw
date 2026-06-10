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
	// resolveTenant maps a user_id to its tenant_id. Used as a
	// last-resort fallback when neither the request body nor the
	// X-Actor-Org-ID header tell us which tenant the user is acting
	// under. Picks the user's oldest tenant — fine for single-tenant
	// users but wrong for multi-tenant ones (the WS session knows the
	// "current" tenant; the HTTP path doesn't, so we lean on header).
	resolveTenant func(ctx context.Context, userID string) (uuid.UUID, error)
	// tenantStore resolves X-Actor-Org-ID (which may be a UUID OR a
	// slug — sheets-mcp passes through whatever the goclaw mcp-bridge
	// injected; that's external_org_id if set, else the tenant slug)
	// into a concrete tenant UUID. Without this, slug-format header
	// values fall through to the resolveTenant fallback above and the
	// orchestrator writes the run with the wrong tenant, which then
	// blocks workflow.event WS delivery to the user's session.
	tenantStore store.TenantStore
}

// NewWorkflowEnqueueHandler constructs the handler. resolveTenant /
// tenantStore are wired separately via With* setters so the inline
// construction site doesn't grow positional args.
func NewWorkflowEnqueueHandler(s store.SheetWorkflowStore, o *runtime.Orchestrator) *WorkflowEnqueueHandler {
	return &WorkflowEnqueueHandler{workflowStore: s, orchestrator: o}
}

// WithTenantResolver returns h with the user_id → tenant_id fallback
// resolver attached.
func (h *WorkflowEnqueueHandler) WithTenantResolver(fn func(ctx context.Context, userID string) (uuid.UUID, error)) *WorkflowEnqueueHandler {
	h.resolveTenant = fn
	return h
}

// WithTenantStore returns h with the tenant store attached — used to
// resolve X-Actor-Org-ID values that arrive as slugs into UUIDs.
func (h *WorkflowEnqueueHandler) WithTenantStore(ts store.TenantStore) *WorkflowEnqueueHandler {
	h.tenantStore = ts
	return h
}

// resolveTenantFromHeader takes the raw X-Actor-Org-ID value (as
// injected by goclaw's mcp-bridge on outbound MCP calls and passed
// through by sidecars) and returns the concrete goclaw tenant UUID.
//
// Header semantics: X-Actor-Org-ID is the web-backend's
// organizations.id (the canonical multi-service identity), stamped on
// tenants.settings.external_org_id by auth-proxy. The MCP bridge
// prefers external_org_id and falls back to the goclaw slug for
// tenants the auth-proxy hasn't touched yet — so this resolver must
// accept BOTH UUID-shaped and slug-shaped values.
//
// Resolution chain — each step VERIFIES the row exists before
// returning, otherwise a parsed-but-nonexistent UUID would slip
// through and produce an FK violation downstream when the
// orchestrator INSERTs sheet_workflows.tenant_id:
//
//  1. UUID-shape: try GetTenantByExternalOrgID first (canonical path).
//  2. UUID-shape: fall through to GetTenant by local id (lets trusted
//     server-side callers pass goclaw's own UUID, e.g. cron).
//  3. Slug-shape (or any non-UUID string): GetTenantBySlug.
//
// Returns uuid.Nil when nothing matches — caller falls back to the
// user_id resolver (or returns 400).
func (h *WorkflowEnqueueHandler) resolveTenantFromHeader(ctx context.Context, value string) uuid.UUID {
	v := strings.TrimSpace(value)
	if v == "" || h.tenantStore == nil {
		return uuid.Nil
	}
	if id, err := uuid.Parse(v); err == nil {
		if t, err := h.tenantStore.GetTenantByExternalOrgID(ctx, v); err == nil && t != nil {
			return t.ID
		}
		if t, err := h.tenantStore.GetTenant(ctx, id); err == nil && t != nil {
			return t.ID
		}
		return uuid.Nil
	}
	if t, err := h.tenantStore.GetTenantBySlug(ctx, v); err == nil && t != nil {
		return t.ID
	}
	return uuid.Nil
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

	// Tenant resolution — three sources, in priority order, each
	// step VERIFIES the candidate id exists in tenants before
	// accepting it. The previous version accepted a parsed UUID
	// blindly, so any caller that happened to forward the
	// X-Actor-Org-ID (which is external_org_id, NOT goclaw's local
	// tenants.id) in the body field would cause an FK violation when
	// the orchestrator INSERTs into sheet_workflows.
	//
	//   1. body `tenant_id` (UUID) — trusted internal caller knew it
	//      AND it points to an existing tenant.
	//   2. X-Actor-Org-ID header — goclaw's mcp-bridge injects this on
	//      every outbound MCP call with the CURRENT tenant the user is
	//      acting under. sheets-mcp passes it through. May be UUID
	//      (external_org_id, canonical) or slug (fallback) —
	//      resolveTenantFromHeader handles both shapes.
	//   3. resolveTenant by user_id — last-resort fallback for callers
	//      that have only the cognito sub. Picks user's oldest tenant
	//      which is WRONG for multi-tenant users; (1) and (2) must
	//      catch the real call path before we hit this.
	if req.TenantID != uuid.Nil && h.tenantStore != nil {
		if t, err := h.tenantStore.GetTenant(ctx, req.TenantID); err != nil || t == nil {
			// Body id is bogus (e.g. external_org_id sent by accident,
			// or stale UUID from a deleted tenant). Discard so we fall
			// through to the header/user resolvers.
			req.TenantID = uuid.Nil
		}
	}
	if req.TenantID == uuid.Nil {
		if hdr := r.Header.Get("X-Actor-Org-ID"); hdr != "" {
			req.TenantID = h.resolveTenantFromHeader(ctx, hdr)
		}
	}
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
