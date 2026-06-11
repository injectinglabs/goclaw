package pg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGSheetWorkflowStore is the Postgres-backed impl of store.SheetWorkflowStore.
// Tables: sheet_workflows, sheet_workflow_runs, sheet_workflow_cells,
// webhook_idempotency (see migration 000073).
type PGSheetWorkflowStore struct {
	db *sql.DB
}

func NewPGSheetWorkflowStore(db *sql.DB) *PGSheetWorkflowStore {
	return &PGSheetWorkflowStore{db: db}
}

// ─── Workflow CRUD ───────────────────────────────────────────────────

func (s *PGSheetWorkflowStore) CreateWorkflow(ctx context.Context, w *store.SheetWorkflow) error {
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	if w.Status == "" {
		w.Status = "active"
	}
	if w.Visibility == "" {
		w.Visibility = "personal"
	}
	colsJSON, err := json.Marshal(w.Columns)
	if err != nil {
		return fmt.Errorf("marshal columns: %w", err)
	}
	trigsJSON, err := json.Marshal(w.Triggers)
	if err != nil {
		return fmt.Errorf("marshal triggers: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sheet_workflows
		  (id, tenant_id, user_id, name, description,
		   spreadsheet_id, sheet_tab, target_range,
		   columns_json, triggers_json, visibility, status,
		   created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5, $6,$7,$8, $9,$10,$11,$12, NOW(), NOW())
	`,
		w.ID, w.TenantID, w.UserID, w.Name, w.Description,
		w.SpreadsheetID, w.SheetTab, w.TargetRange,
		colsJSON, trigsJSON, w.Visibility, w.Status,
	)
	return err
}

func (s *PGSheetWorkflowStore) GetWorkflow(ctx context.Context, id uuid.UUID) (*store.SheetWorkflow, error) {
	w := &store.SheetWorkflow{}
	var colsJSON, trigsJSON []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, user_id, name, description,
		       spreadsheet_id, sheet_tab, target_range,
		       columns_json, triggers_json, visibility, status,
		       last_run_at, created_at, updated_at
		  FROM sheet_workflows
		 WHERE id = $1
	`, id).Scan(
		&w.ID, &w.TenantID, &w.UserID, &w.Name, &w.Description,
		&w.SpreadsheetID, &w.SheetTab, &w.TargetRange,
		&colsJSON, &trigsJSON, &w.Visibility, &w.Status,
		&w.LastRunAt, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(colsJSON, &w.Columns); err != nil {
		return nil, fmt.Errorf("unmarshal columns: %w", err)
	}
	if err := json.Unmarshal(trigsJSON, &w.Triggers); err != nil {
		return nil, fmt.Errorf("unmarshal triggers: %w", err)
	}
	return w, nil
}

func (s *PGSheetWorkflowStore) ListWorkflowsForUser(ctx context.Context, tenantID uuid.UUID, userID, role string) ([]store.SheetWorkflow, error) {
	// owner/admin see everything in tenant; member sees own + team-shared
	// from other members. Same visibility shape as skills / agents.
	var rows *sql.Rows
	var err error
	if role == "owner" || role == "admin" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, tenant_id, user_id, name, description,
			       spreadsheet_id, sheet_tab, target_range,
			       columns_json, triggers_json, visibility, status,
			       last_run_at, created_at, updated_at
			  FROM sheet_workflows
			 WHERE tenant_id = $1
			 ORDER BY updated_at DESC
		`, tenantID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, tenant_id, user_id, name, description,
			       spreadsheet_id, sheet_tab, target_range,
			       columns_json, triggers_json, visibility, status,
			       last_run_at, created_at, updated_at
			  FROM sheet_workflows
			 WHERE tenant_id = $1
			   AND (user_id = $2 OR visibility = 'team')
			 ORDER BY updated_at DESC
		`, tenantID, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.SheetWorkflow
	for rows.Next() {
		w := store.SheetWorkflow{}
		var colsJSON, trigsJSON []byte
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.UserID, &w.Name, &w.Description,
			&w.SpreadsheetID, &w.SheetTab, &w.TargetRange,
			&colsJSON, &trigsJSON, &w.Visibility, &w.Status,
			&w.LastRunAt, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(colsJSON, &w.Columns)
		_ = json.Unmarshal(trigsJSON, &w.Triggers)
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *PGSheetWorkflowStore) UpdateWorkflow(ctx context.Context, w *store.SheetWorkflow) error {
	colsJSON, err := json.Marshal(w.Columns)
	if err != nil {
		return fmt.Errorf("marshal columns: %w", err)
	}
	trigsJSON, err := json.Marshal(w.Triggers)
	if err != nil {
		return fmt.Errorf("marshal triggers: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE sheet_workflows
		   SET name = $2, description = $3,
		       spreadsheet_id = $4, sheet_tab = $5, target_range = $6,
		       columns_json = $7, triggers_json = $8,
		       visibility = $9, status = $10,
		       updated_at = NOW()
		 WHERE id = $1
	`,
		w.ID, w.Name, w.Description,
		w.SpreadsheetID, w.SheetTab, w.TargetRange,
		colsJSON, trigsJSON, w.Visibility, w.Status,
	)
	return err
}

func (s *PGSheetWorkflowStore) DeleteWorkflow(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sheet_workflows WHERE id = $1`, id)
	return err
}

// RotateWebhookToken generates a new random 32-byte token, stores its
// SHA256 hash in the workflow's triggers_json (replacing any existing
// webhook trigger's token_hash), and returns the raw token for the
// caller to show to the user ONCE. Subsequent reads from DB return only
// the hash.
func (s *PGSheetWorkflowStore) RotateWebhookToken(ctx context.Context, workflowID uuid.UUID) (string, error) {
	// 32 random bytes → 64 hex chars
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	rawHex := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(rawHex))
	tokenHash := hex.EncodeToString(sum[:])

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var trigsJSON []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT triggers_json FROM sheet_workflows WHERE id = $1 FOR UPDATE`,
		workflowID,
	).Scan(&trigsJSON); err != nil {
		return "", err
	}
	var trigs []store.SheetWorkflowTrigger
	if err := json.Unmarshal(trigsJSON, &trigs); err != nil {
		return "", fmt.Errorf("unmarshal triggers: %w", err)
	}

	// Update existing webhook trigger or append new one.
	updated := false
	for i := range trigs {
		if trigs[i].Type == "webhook" {
			trigs[i].TokenHash = tokenHash
			updated = true
			break
		}
	}
	if !updated {
		trigs = append(trigs, store.SheetWorkflowTrigger{
			Type:      "webhook",
			TokenHash: tokenHash,
		})
	}
	newJSON, err := json.Marshal(trigs)
	if err != nil {
		return "", fmt.Errorf("marshal triggers: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sheet_workflows SET triggers_json = $2, updated_at = NOW() WHERE id = $1`,
		workflowID, newJSON,
	); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return rawHex, nil
}

// ─── Run lifecycle ───────────────────────────────────────────────────

func (s *PGSheetWorkflowStore) CreateRun(ctx context.Context, r *store.SheetWorkflowRun) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.Status == "" {
		r.Status = "queued"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sheet_workflow_runs
		  (id, workflow_id, tenant_id, triggered_by, trigger_payload,
		   status, row_count, completed_count, error_count,
		   created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5, $6,$7,$8,$9, NOW(), NOW())
	`,
		r.ID, r.WorkflowID, r.TenantID, r.TriggeredBy, r.TriggerPayload,
		r.Status, r.RowCount, r.CompletedCount, r.ErrorCount,
	)
	return err
}

func (s *PGSheetWorkflowStore) GetRun(ctx context.Context, id uuid.UUID) (*store.SheetWorkflowRun, error) {
	r := &store.SheetWorkflowRun{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, workflow_id, tenant_id, triggered_by, trigger_payload,
		       status, row_count, completed_count, error_count, error_message,
		       total_tokens_in, total_tokens_out,
		       started_at, finished_at, created_at, updated_at
		  FROM sheet_workflow_runs
		 WHERE id = $1
	`, id).Scan(
		&r.ID, &r.WorkflowID, &r.TenantID, &r.TriggeredBy, &r.TriggerPayload,
		&r.Status, &r.RowCount, &r.CompletedCount, &r.ErrorCount, &r.ErrorMessage,
		&r.TotalTokensIn, &r.TotalTokensOut,
		&r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *PGSheetWorkflowStore) ListRunsForWorkflow(ctx context.Context, workflowID uuid.UUID, limit int) ([]store.SheetWorkflowRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workflow_id, tenant_id, triggered_by, trigger_payload,
		       status, row_count, completed_count, error_count, error_message,
		       total_tokens_in, total_tokens_out,
		       started_at, finished_at, created_at, updated_at
		  FROM sheet_workflow_runs
		 WHERE workflow_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2
	`, workflowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.SheetWorkflowRun
	for rows.Next() {
		r := store.SheetWorkflowRun{}
		if err := rows.Scan(
			&r.ID, &r.WorkflowID, &r.TenantID, &r.TriggeredBy, &r.TriggerPayload,
			&r.Status, &r.RowCount, &r.CompletedCount, &r.ErrorCount, &r.ErrorMessage,
			&r.TotalTokensIn, &r.TotalTokensOut,
			&r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PGSheetWorkflowStore) UpdateRunProgress(ctx context.Context, runID uuid.UUID, completed, errored int, tokensIn, tokensOut int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sheet_workflow_runs
		   SET completed_count = $2,
		       error_count = $3,
		       total_tokens_in = $4,
		       total_tokens_out = $5,
		       status = CASE
		         WHEN status = 'queued' THEN 'running'
		         ELSE status
		       END,
		       started_at = COALESCE(started_at, NOW()),
		       updated_at = NOW()
		 WHERE id = $1
	`, runID, completed, errored, tokensIn, tokensOut)
	return err
}

func (s *PGSheetWorkflowStore) SumRunCellTokens(ctx context.Context, runID uuid.UUID) (int, int, error) {
	var tokensIn, tokensOut int
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		  FROM sheet_workflow_cells
		 WHERE run_id = $1
	`, runID).Scan(&tokensIn, &tokensOut)
	return tokensIn, tokensOut, err
}

func (s *PGSheetWorkflowStore) FinishRun(ctx context.Context, runID uuid.UUID, status string, errorMessage *string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE sheet_workflow_runs
		   SET status = $2,
		       error_message = $3,
		       finished_at = NOW(),
		       updated_at = NOW()
		 WHERE id = $1
	`, runID, status, errorMessage); err != nil {
		return err
	}
	// Bump parent workflow's last_run_at for ephemeral-cleanup vacuum.
	if _, err := tx.ExecContext(ctx, `
		UPDATE sheet_workflows
		   SET last_run_at = NOW(),
		       updated_at = NOW()
		 WHERE id = (SELECT workflow_id FROM sheet_workflow_runs WHERE id = $1)
	`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// ─── Cell ops ────────────────────────────────────────────────────────

func (s *PGSheetWorkflowStore) BulkInitCells(ctx context.Context, runID uuid.UUID, rowCount, colCount int) error {
	if rowCount == 0 || colCount == 0 {
		return nil
	}
	// Build a multi-row INSERT in chunks of 1000 cells to keep statements
	// reasonable in size for large sheets.
	const chunkSize = 1000
	total := rowCount * colCount
	for off := 0; off < total; off += chunkSize {
		end := off + chunkSize
		if end > total {
			end = total
		}
		args := make([]interface{}, 0, (end-off)*3+1)
		args = append(args, runID)
		var b []byte
		b = append(b, "INSERT INTO sheet_workflow_cells (run_id, row_idx, col_idx) VALUES "...)
		for i := off; i < end; i++ {
			row := i / colCount
			col := i % colCount
			if i > off {
				b = append(b, ',')
			}
			n := len(args)
			b = append(b, fmt.Sprintf("($1,$%d,$%d)", n+1, n+2)...)
			args = append(args, row, col)
		}
		b = append(b, " ON CONFLICT (run_id, row_idx, col_idx) DO NOTHING"...)
		if _, err := s.db.ExecContext(ctx, string(b), args...); err != nil {
			return err
		}
	}
	// Reflect total expected row count on the run row for progress UI.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sheet_workflow_runs SET row_count = $2, updated_at = NOW() WHERE id = $1`,
		runID, rowCount,
	); err != nil {
		return err
	}
	return nil
}

func (s *PGSheetWorkflowStore) UpdateCellStatus(ctx context.Context, runID uuid.UUID, rowIdx, colIdx int, status string, errMsg *string, attempt int, tokensIn, tokensOut int, latencyMs *int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sheet_workflow_cells
		   SET status = $4,
		       error_message = $5,
		       attempt = $6,
		       tokens_in = $7,
		       tokens_out = $8,
		       latency_ms = $9,
		       updated_at = NOW()
		 WHERE run_id = $1 AND row_idx = $2 AND col_idx = $3
	`, runID, rowIdx, colIdx, status, errMsg, attempt, tokensIn, tokensOut, latencyMs)
	return err
}

func (s *PGSheetWorkflowStore) ListUnfinishedCells(ctx context.Context, runID uuid.UUID) ([]store.SheetWorkflowCell, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, row_idx, col_idx, status, error_message, attempt,
		       tokens_in, tokens_out, latency_ms, updated_at
		  FROM sheet_workflow_cells
		 WHERE run_id = $1
		   AND status IN ('queued', 'running')
		 ORDER BY row_idx, col_idx
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.SheetWorkflowCell
	for rows.Next() {
		c := store.SheetWorkflowCell{}
		if err := rows.Scan(
			&c.RunID, &c.RowIdx, &c.ColIdx, &c.Status, &c.ErrorMessage, &c.Attempt,
			&c.TokensIn, &c.TokensOut, &c.LatencyMs, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListAllCells returns every cell for a run regardless of status, in
// (row_idx, col_idx) order. Used by workflow.runState to rehydrate the
// SPA's split-view canvas after a WS reconnect — `done` and `error`
// cells must be included so the grid renders the actual progress, not
// just the unfinished tail.
func (s *PGSheetWorkflowStore) ListAllCells(ctx context.Context, runID uuid.UUID) ([]store.SheetWorkflowCell, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, row_idx, col_idx, status, error_message, attempt,
		       tokens_in, tokens_out, latency_ms, updated_at
		  FROM sheet_workflow_cells
		 WHERE run_id = $1
		 ORDER BY row_idx, col_idx
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.SheetWorkflowCell
	for rows.Next() {
		c := store.SheetWorkflowCell{}
		if err := rows.Scan(
			&c.RunID, &c.RowIdx, &c.ColIdx, &c.Status, &c.ErrorMessage, &c.Attempt,
			&c.TokensIn, &c.TokensOut, &c.LatencyMs, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ─── Recovery ────────────────────────────────────────────────────────

// ListRecoverableRuns returns runs in queued/running state whose row was
// last updated longer than `olderThan` ago — these are candidates for
// the recovery scanner to resume after an instance crash.
func (s *PGSheetWorkflowStore) ListRecoverableRuns(ctx context.Context, olderThan time.Duration) ([]store.SheetWorkflowRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workflow_id, tenant_id, triggered_by, trigger_payload,
		       status, row_count, completed_count, error_count, error_message,
		       total_tokens_in, total_tokens_out,
		       started_at, finished_at, created_at, updated_at
		  FROM sheet_workflow_runs
		 WHERE status IN ('queued', 'running')
		   AND updated_at < NOW() - ($1::int * INTERVAL '1 second')
		 ORDER BY started_at NULLS FIRST
	`, int(olderThan.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.SheetWorkflowRun
	for rows.Next() {
		r := store.SheetWorkflowRun{}
		if err := rows.Scan(
			&r.ID, &r.WorkflowID, &r.TenantID, &r.TriggeredBy, &r.TriggerPayload,
			&r.Status, &r.RowCount, &r.CompletedCount, &r.ErrorCount, &r.ErrorMessage,
			&r.TotalTokensIn, &r.TotalTokensOut,
			&r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ─── Webhook idempotency ─────────────────────────────────────────────

// ClaimIdempotencyKey atomically tries to register a new key for a
// workflow. If the key already exists, returns alreadySeen=true and the
// prior run_id (may be nil if the prior attempt failed before enqueue).
// Caller responsibility: when alreadySeen=false, follow up with
// BindIdempotencyRun once the run is created.
func (s *PGSheetWorkflowStore) ClaimIdempotencyKey(ctx context.Context, key string, workflowID uuid.UUID) (bool, *uuid.UUID, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()

	var existingRun *uuid.UUID
	err = tx.QueryRowContext(ctx,
		`SELECT run_id FROM webhook_idempotency WHERE key = $1 FOR UPDATE`,
		key,
	).Scan(&existingRun)
	if err == nil {
		// Key already claimed — caller should return cached run_id.
		if cerr := tx.Commit(); cerr != nil {
			return false, nil, cerr
		}
		return true, existingRun, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, nil, err
	}

	// First-time claim. Insert a placeholder row (run_id NULL until enqueue).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webhook_idempotency (key, workflow_id) VALUES ($1, $2)`,
		key, workflowID,
	); err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return false, nil, nil
}

func (s *PGSheetWorkflowStore) BindIdempotencyRun(ctx context.Context, key string, runID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_idempotency SET run_id = $2 WHERE key = $1`,
		key, runID,
	)
	return err
}

func (s *PGSheetWorkflowStore) SweepExpiredIdempotency(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_idempotency WHERE expires_at < $1`,
		now,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
