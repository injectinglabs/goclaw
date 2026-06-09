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
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
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
	store store.SheetWorkflowStore
}

// NewWorkflowMethods constructs a WorkflowMethods backed by the
// provided sheet-workflow store. Callers wire this only when the
// orchestrator is enabled (workflowStore != nil in gateway_http_wiring).
func NewWorkflowMethods(s store.SheetWorkflowStore) *WorkflowMethods {
	return &WorkflowMethods{store: s}
}

// Register adds workflow methods to the WS RPC router.
func (m *WorkflowMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodWorkflowRunState, m.handleRunState)
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
