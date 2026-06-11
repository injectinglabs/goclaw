// Package methods — sheet-workflow related WS JSON-RPC methods.
//
// Currently exposes `workflow.runState` — the SPA-facing reconnect
// rehydration endpoint for the Paradigm-style split-view sheet canvas.
// `workflow.event` (the live broadcast bus) is at-least-once but not
// durable; anything emitted before the SPA (re)connected is gone, so
// the canvas calls this method on every connect for each active run
// it knows about (from IndexedDB snapshot) and folds the snapshot into
// the store before resuming live event consumption.
//
// Tenant scoping: this method uses the same WS session identity as
// chat.send / chat.activeSessions — caller's tenantID must equal the
// run's TenantID, otherwise NOT_FOUND (we never leak existence of
// cross-tenant runs).
//
// Contract is locked in goclaw/docs/SHEET_WORKFLOWS_EVENTS.md.
package methods

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/workflow/runtime"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// WorkflowMethods exposes sheet-workflow WS RPC methods.
//
// The handler set is intentionally tiny — orchestration / enqueue
// lives on the HTTP path (POST /v1/internal/workflows/enqueue, called
// by the sheets_enrich_run MCP tool + cron + webhooks). The WS surface
// only serves the SPA's read-side needs: today, the per-run snapshot.
// CRUD (list workflows / templates / scheduled jobs) will land here in
// Phase B and continue the same pattern.
type WorkflowMethods struct {
	store   store.SheetWorkflowStore
	enqueue *httpapi.WorkflowEnqueueHandler
	reader  *runtime.MCPSheetReader
	// bus exposes the per-run resume ring stamped at publish time.
	// Used by workflow.runsSubscribe to replay events the SPA missed
	// while disconnected. Nil → resume disabled (RPC returns empty
	// events array, same as if the run had no buffered events).
	bus *runtime.BusEventBus
}

// NewWorkflowMethods constructs a WorkflowMethods backed by the
// provided sheet-workflow store. Callers wire this only when the
// orchestrator is enabled (workflowStore != nil in gateway_http_wiring).
//
// enqueue is the HTTP enqueue handler — we reuse its EnqueueAsUser
// core logic for the workflow.enqueue WS RPC so both code paths share
// validation, workflow create-on-the-fly, and orchestrator wiring.
// Pass nil to disable the WS enqueue RPC (read-only methods stay live).
//
// reader is a composio-mcp client used by `workflow.peekSheet` to read
// the user's Google Sheet contents directly (source of truth). Pass nil
// to disable that RPC; read-only methods stay live.
//
// bus is the orchestrator's BusEventBus; workflow.runsSubscribe reads
// from it. Pass nil to disable that RPC.
func NewWorkflowMethods(s store.SheetWorkflowStore, enqueue *httpapi.WorkflowEnqueueHandler, reader *runtime.MCPSheetReader, bus *runtime.BusEventBus) *WorkflowMethods {
	return &WorkflowMethods{store: s, enqueue: enqueue, reader: reader, bus: bus}
}

// Register adds workflow methods to the WS RPC router.
func (m *WorkflowMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodWorkflowRunState, m.handleRunState)
	if m.enqueue != nil {
		router.Register(protocol.MethodWorkflowEnqueue, m.handleEnqueue)
	}
	if m.reader != nil {
		router.Register(protocol.MethodWorkflowPeekSheet, m.handlePeekSheet)
	}
	if m.bus != nil {
		router.Register(protocol.MethodWorkflowRunsSubscribe, m.handleRunsSubscribe)
	}
}

// handleRunsSubscribe is the resumable-stream replay endpoint for
// sheet-workflow runs. The client supplies (run_id, since_seq); we
// return every buffered event whose Seq > since_seq, in emit order.
// Live events arriving after the response continue through the normal
// workflow.event broadcast, so the client receives the gap-fill AND
// the live tail seamlessly. Mirrors chat's runs.subscribe contract.
//
// Tenant scoping: BusEventBus only buffers events whose RunEvent
// already carries the right TenantID, but we do not return events to
// callers whose session tenant doesn't match — the per-event filter
// in gateway/event_filter.go does the same check on live broadcast,
// applying the same rule here closes the resume path symmetrically.
func (m *WorkflowMethods) handleRunsSubscribe(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params struct {
		RunID    string `json:"run_id"`
		SinceSeq int64  `json:"since_seq"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.RunID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "run_id")))
		return
	}
	runID, err := uuid.Parse(params.RunID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "run_id must be a UUID"))
		return
	}
	events := m.bus.EventsSince(runID, params.SinceSeq)
	// Filter to caller's tenant + user — symmetric with live broadcast.
	tid := client.TenantID()
	uid := client.UserID()
	out := make([]runtime.RunEvent, 0, len(events))
	for _, e := range events {
		if e.TenantID != tid {
			continue
		}
		if e.UserID != "" && e.UserID != uid {
			continue
		}
		out = append(out, e)
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"run_id": params.RunID,
		"events": out,
	}))
}

// runStateCell is the per-cell payload mirrored 1:1 with the TS
// schema in docs/SHEET_WORKFLOWS_EVENTS.md. Status values follow the
// SheetWorkflowCell.Status vocabulary (queued|running|done|error|skipped).
type runStateCell struct {
	RowIdx       int     `json:"row_idx"`
	ColIdx       int     `json:"col_idx"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"error_message,omitempty"`
	Attempt      int     `json:"attempt"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	LatencyMs    *int    `json:"latency_ms,omitempty"`
}

// runStateRun mirrors a (filtered) view of store.SheetWorkflowRun for
// the SPA. We expose only fields the canvas actually renders — token
// totals stay on the per-cell records the orchestrator already emits
// over the WS bus, no need to re-publish them at run level.
type runStateRun struct {
	ID             uuid.UUID `json:"id"`
	WorkflowID     uuid.UUID `json:"workflow_id"`
	Status         string    `json:"status"`
	RowCount       int       `json:"row_count"`
	CompletedCount int       `json:"completed_count"`
	ErrorCount     int       `json:"error_count"`
	ErrorMessage   *string   `json:"error_message,omitempty"`
	StartedAt      *string   `json:"started_at,omitempty"`
	FinishedAt     *string   `json:"finished_at,omitempty"`
	// Sheet metadata lifted from the parent workflow row so the SPA
	// chip can call workflow.peekSheet after a reload without scraping
	// the (display-truncated) tool-result text. Mirrors the same
	// fields run.started carries on the live workflow.event stream.
	SpreadsheetID string `json:"spreadsheet_id,omitempty"`
	SheetTab      string `json:"sheet_tab,omitempty"`
	WorkflowName  string `json:"workflow_name,omitempty"`
	// Cumulative enrichment token cost (all cells, all search round-trips).
	// Lets the chip restore the total-cost display after a reload.
	TokensIn  int `json:"tokens_in,omitempty"`
	TokensOut int `json:"tokens_out,omitempty"`
}

// handleRunState returns the full per-cell snapshot for one run.
//
// Request:  { "run_id": "<uuid>" }
// Response: { "run": <runStateRun>, "cells": [<runStateCell>...] }
//
// Errors:
//   - INVALID_REQUEST — run_id missing or not a UUID
//   - NOT_FOUND       — run doesn't exist OR belongs to another tenant
//     (combined deliberately so we don't leak run existence across
//     tenants)
//   - INTERNAL        — DB unreachable / unexpected
func (m *WorkflowMethods) handleRunState(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)

	var params struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.RunID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "run_id")))
		return
	}
	runID, err := uuid.Parse(params.RunID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "run_id must be a UUID"))
		return
	}

	run, err := m.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, "run not found"))
			return
		}
		slog.Error("workflow.runState GetRun failed", "run_id", runID, "err", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to load run"))
		return
	}
	if run == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, "run not found"))
		return
	}

	// Tenant scope — collapsing mismatch to NOT_FOUND so the SPA can't
	// probe for the existence of runs in other tenants.
	if run.TenantID != client.TenantID() {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, "run not found"))
		return
	}

	cells, err := m.store.ListAllCells(ctx, runID)
	if err != nil {
		slog.Error("workflow.runState ListAllCells failed", "run_id", runID, "err", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to load cells"))
		return
	}

	out := make([]runStateCell, 0, len(cells))
	for _, c := range cells {
		entry := runStateCell{
			RowIdx:    c.RowIdx,
			ColIdx:    c.ColIdx,
			Status:    c.Status,
			Attempt:   c.Attempt,
			TokensIn:  c.TokensIn,
			TokensOut: c.TokensOut,
		}
		if c.ErrorMessage != nil {
			s := *c.ErrorMessage
			entry.ErrorMessage = &s
		}
		if c.LatencyMs != nil {
			n := *c.LatencyMs
			entry.LatencyMs = &n
		}
		out = append(out, entry)
	}

	resp := runStateRun{
		ID:             run.ID,
		WorkflowID:     run.WorkflowID,
		Status:         run.Status,
		RowCount:       run.RowCount,
		CompletedCount: run.CompletedCount,
		ErrorCount:     run.ErrorCount,
		ErrorMessage:   run.ErrorMessage,
		// Authoritative cumulative token cost from the DB — what lets the
		// chip + parent bubble restore the real total after a reload
		// instead of resetting to 0.
		TokensIn:  run.TotalTokensIn,
		TokensOut: run.TotalTokensOut,
	}
	// Best-effort: a missing workflow row (deleted after the run) just
	// leaves the sheet metadata empty — the chip falls back to its
	// tool-result-derived props, same as before this field existed.
	if wf, err := m.store.GetWorkflow(ctx, run.WorkflowID); err == nil && wf != nil {
		resp.SpreadsheetID = wf.SpreadsheetID
		resp.SheetTab = wf.SheetTab
		resp.WorkflowName = wf.Name
	}
	if run.StartedAt != nil {
		s := run.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		resp.StartedAt = &s
	}
	if run.FinishedAt != nil {
		s := run.FinishedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		resp.FinishedAt = &s
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"run":   resp,
		"cells": out,
	}))
}

// handleEnqueue kicks off a new sheet-workflow run on behalf of the
// caller. SPA-facing entry point for the Enrich wizard. Mirrors the
// HTTP /v1/internal/workflows/enqueue contract but reads tenant + user
// from the WS session (never trusts client-supplied identity).
//
// Request body (params):
//
//	{
//	  // when set, uses an existing saved workflow's schema:
//	  "workflow_id": "<uuid>",
//
//	  // OR inline ad-hoc enrichment:
//	  "name":           "Q3 prospects — CEO + LinkedIn",
//	  "spreadsheet_id": "<google sheet id>",
//	  "sheet_tab":      "Sheet1",
//	  "target_range":   "A2:Z",
//	  "columns": [
//	    { "id": "ceo", "name": "CEO", "prompt": "...", "type": "text",
//	      "target_col": "B", "depends_on": [] },
//	    ...
//	  ],
//
//	  // common — keyed by row index, each map is the per-row context
//	  // (column id → value).
//	  "rows": { "0": {"company": "OpenAI"}, "1": {"company": "Anthropic"} },
//
//	  "triggered_by":   "manual",            // default
//	  "max_concurrent": 8                    // optional
//	}
//
// Response: { "run_id": "<uuid>", "status": "queued" }
//
// Errors: INVALID_REQUEST (bad body) | INTERNAL (orchestrator failure).
func (m *WorkflowMethods) handleEnqueue(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)

	if m.enqueue == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "workflow runtime not configured"))
		return
	}

	var body httpapi.EnqueueRequest
	if err := json.Unmarshal(req.Params, &body); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid json: "+err.Error()))
		return
	}

	tenantID := client.TenantID()
	userID := client.UserID()
	if tenantID == uuid.Nil || userID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "session")))
		return
	}

	// Defaults — SPA users should never have to set these.
	if body.TriggeredBy == "" {
		body.TriggeredBy = "manual"
	}

	runID, err := m.enqueue.EnqueueAsUser(ctx, tenantID, userID, &body)
	if err != nil {
		// Validation errors are user-actionable → INVALID_REQUEST. Real
		// internal failures (orchestrator panics, DB unreachable) are
		// INTERNAL. Crude heuristic: anything wrapped with "start run:"
		// or "workflow store:" is on us; the rest is validation.
		msg := err.Error()
		code := protocol.ErrInvalidRequest
		if isInternalEnqueueError(msg) {
			code = protocol.ErrInternal
			slog.Error("workflow.enqueue failed", "tenant", tenantID, "user", userID, "err", err)
		}
		client.SendResponse(protocol.NewErrorResponse(req.ID, code, msg))
		return
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"run_id": runID,
		"status": "queued",
	}))
}

// isInternalEnqueueError classifies enqueue errors as user-actionable
// (validation) vs server-side (orchestrator / DB). Used to pick
// INVALID_REQUEST vs INTERNAL for the WS error code surface.
func isInternalEnqueueError(msg string) bool {
	// Errors prefixed with "start run:" come from orchestrator.StartRun;
	// "workflow store:" from store ops. Both are server-side. Validation
	// errors come back bare (e.g. "rows must not be empty").
	if len(msg) >= 10 && msg[:10] == "start run:" {
		return true
	}
	if len(msg) >= 15 && msg[:15] == "workflow store:" {
		return true
	}
	return false
}

// handlePeekSheet reads a range from the caller's Google Sheet and
// returns the values as a 2-D string grid. SPA bubble uses this to
// render the actual contents of the user's sheet — what they would
// see if they opened it in Google Sheets directly.
//
// Request:  { "spreadsheet_id": "...", "sheet_tab": "...", "range": "A1:E10" }
//   - sheet_tab is optional; defaults to "Sheet1"
//   - range is in A1 notation; values inside the tab (e.g. "A1:E10")
//
// Response: { "values": [["company","country",...], ["OpenAI",...], ...] }
//
// Auth: uses caller's WS userID as X-Proxy-User to composio. Google's
// own OAuth then dictates access — we don't add a goclaw-side tenant
// check because the user's Google identity already scopes this.
func (m *WorkflowMethods) handlePeekSheet(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	if m.reader == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "sheet reader not configured"))
		return
	}

	var params struct {
		SpreadsheetID string `json:"spreadsheet_id"`
		SheetTab      string `json:"sheet_tab"`
		Range         string `json:"range"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid json: "+err.Error()))
		return
	}
	if params.SpreadsheetID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "spreadsheet_id")))
		return
	}
	tab := params.SheetTab
	if tab == "" {
		tab = "Sheet1"
	}
	a1 := params.Range
	if a1 == "" {
		// Default range covers a reasonable bulk-enrich sheet without
		// loading 1000 rows. Caller can widen via the param.
		a1 = "A1:Z100"
	}
	// Quote the tab name in case it has spaces (e.g. "Top AI Companies").
	fullRange := "'" + tab + "'!" + a1

	userID := client.UserID()
	if userID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "no user identity on session"))
		return
	}

	values, err := m.reader.ReadRange(ctx, userID, params.SpreadsheetID, fullRange)
	if err != nil {
		slog.Error("workflow.peekSheet failed", "spreadsheet_id", params.SpreadsheetID, "range", fullRange, "err", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "sheet read failed: "+err.Error()))
		return
	}

	if values == nil {
		values = [][]string{}
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"spreadsheet_id": params.SpreadsheetID,
		"sheet_tab":      tab,
		"range":          a1,
		"values":         values,
	}))
}
