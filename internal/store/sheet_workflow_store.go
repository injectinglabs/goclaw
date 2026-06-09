package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ─── Workflow definition ─────────────────────────────────────────────

// SheetWorkflow is a persistent definition that binds a Google Sheet to
// a typed column schema plus a set of triggers (manual / cron / webhook).
// Persisted in `sheet_workflows`. Mutations come from the workflow CRUD
// MCP tools (`sheets_workflow_*`) — the orchestrator only reads.
type SheetWorkflow struct {
	ID          uuid.UUID `json:"id"           db:"id"`
	TenantID    uuid.UUID `json:"tenant_id"    db:"tenant_id"`
	UserID      string    `json:"user_id"      db:"user_id"`
	Name        string    `json:"name"         db:"name"`
	Description *string   `json:"description,omitempty" db:"description"`

	SpreadsheetID string `json:"spreadsheet_id" db:"spreadsheet_id"`
	SheetTab      string `json:"sheet_tab"      db:"sheet_tab"`
	TargetRange   string `json:"target_range"   db:"target_range"`

	// Typed column schema. Each entry: {id, name, prompt, type, depends_on:[id...]}.
	// Validated server-side; the agent only ever produces this via structured
	// output forced by the `sheets_workflow_create` tool's schema.
	Columns []SheetWorkflowColumn `json:"columns" db:"-"`

	// Trigger list. Each entry is one of:
	//   {type: "cron",    expr: "0 9 * * MON"}
	//   {type: "webhook", token_hash: "...", payload_map: {...}}
	// `type:"manual"` is implicit — never stored.
	Triggers []SheetWorkflowTrigger `json:"triggers" db:"-"`

	Visibility string `json:"visibility" db:"visibility"` // personal | team
	Status     string `json:"status"     db:"status"`     // active | paused | broken

	LastRunAt *time.Time `json:"last_run_at,omitempty" db:"last_run_at"`
	CreatedAt time.Time  `json:"created_at"            db:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"            db:"updated_at"`
}

// SheetWorkflowColumn describes one column in the workflow's schema.
// Type values mirror Paradigm's column-type vocabulary so the LLM has
// a familiar surface. `depends_on` lists column IDs whose value this
// column's prompt needs as context (e.g. "CEO LinkedIn" depends on "CEO").
type SheetWorkflowColumn struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prompt     string   `json:"prompt"`
	Type       string   `json:"type"`        // text | number | url | email | checkbox | select | multi_select
	DependsOn  []string `json:"depends_on,omitempty"`
	// For select / multi_select: enum of allowed values.
	Options    []string `json:"options,omitempty"`
	// TargetCol is the A1 column letter the orchestrator writes this
	// cell into (e.g. "B", "C", "AA"). Required when the agent leaves
	// the first column(s) pre-filled — without it the orchestrator
	// falls back to position-in-the-columns-list and ends up writing
	// to A..N, overwriting the seed values the agent just appended.
	// Optional for back-compat: empty → position-based mapping.
	TargetCol  string   `json:"target_col,omitempty"`
}

// SheetWorkflowTrigger is a polymorphic record — the `Type` field
// determines which other fields are populated. Decoded from jsonb.
type SheetWorkflowTrigger struct {
	Type string `json:"type"` // cron | webhook

	// type=cron
	CronExpr string `json:"expr,omitempty"`

	// type=webhook
	// SHA256(token) hex — raw token is shown only once at create/rotate.
	TokenHash string `json:"token_hash,omitempty"`
	// JSONPath → column mapping for incoming payload, e.g.
	//   {"company": "$.lead.company", "contact": "$.lead.email"}
	// Default (empty) = identity-by-name match against column names.
	PayloadMap map[string]string `json:"payload_map,omitempty"`
}

// ─── Run state ──────────────────────────────────────────────────────

// SheetWorkflowRun is one execution of a workflow. Persisted so a
// crashed orchestrator can resume mid-flight (see RecoveryScanner).
// One row per manual/cron/webhook invocation.
type SheetWorkflowRun struct {
	ID            uuid.UUID `json:"id"            db:"id"`
	WorkflowID    uuid.UUID `json:"workflow_id"   db:"workflow_id"`
	TenantID      uuid.UUID `json:"tenant_id"     db:"tenant_id"`

	TriggeredBy     string `json:"triggered_by"   db:"triggered_by"` // manual | cron | webhook | retry
	TriggerPayload  []byte `json:"-"              db:"trigger_payload"` // raw jsonb

	Status          string `json:"status" db:"status"` // queued | running | done | cancelled | error
	RowCount        int    `json:"row_count"       db:"row_count"`
	CompletedCount  int    `json:"completed_count" db:"completed_count"`
	ErrorCount      int    `json:"error_count"     db:"error_count"`

	ErrorMessage *string `json:"error_message,omitempty" db:"error_message"`

	TotalTokensIn  int `json:"total_tokens_in"  db:"total_tokens_in"`
	TotalTokensOut int `json:"total_tokens_out" db:"total_tokens_out"`

	StartedAt  *time.Time `json:"started_at,omitempty"  db:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty" db:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"            db:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"            db:"updated_at"`
}

// SheetWorkflowCell is per-(run, row, col) state. The cell *value* is
// NOT stored here — source of truth is the Google Sheet. We only keep
// metadata needed for retry, recovery, and the live grid in the SPA.
type SheetWorkflowCell struct {
	RunID  uuid.UUID `json:"run_id"  db:"run_id"`
	RowIdx int       `json:"row_idx" db:"row_idx"`
	ColIdx int       `json:"col_idx" db:"col_idx"`

	Status       string  `json:"status"        db:"status"` // queued | running | done | error | skipped
	ErrorMessage *string `json:"error_message,omitempty" db:"error_message"`
	Attempt      int     `json:"attempt"       db:"attempt"`

	TokensIn  int  `json:"tokens_in"  db:"tokens_in"`
	TokensOut int  `json:"tokens_out" db:"tokens_out"`
	LatencyMs *int `json:"latency_ms,omitempty" db:"latency_ms"`

	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// ─── Webhook idempotency ────────────────────────────────────────────

// WebhookIdempotencyKey records a webhook invocation so that retries
// (Zapier / HubSpot / Make all retry on 5xx) don't enqueue duplicate runs.
// TTL = 24h, swept hourly.
type WebhookIdempotencyKey struct {
	Key        string     `json:"key"          db:"key"`
	WorkflowID uuid.UUID  `json:"workflow_id"  db:"workflow_id"`
	RunID      *uuid.UUID `json:"run_id,omitempty" db:"run_id"`
	CreatedAt  time.Time  `json:"created_at"   db:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"   db:"expires_at"`
}

// ─── Store interface ────────────────────────────────────────────────

// SheetWorkflowStore is the persistence boundary for the sheet-workflows
// subsystem. Concrete impl in pg/ (PR2). Split into focused groups so
// future tests can mock just the slice they need.
type SheetWorkflowStore interface {
	// Workflow CRUD
	CreateWorkflow(ctx context.Context, w *SheetWorkflow) error
	GetWorkflow(ctx context.Context, id uuid.UUID) (*SheetWorkflow, error)
	ListWorkflowsForUser(ctx context.Context, tenantID uuid.UUID, userID, role string) ([]SheetWorkflow, error)
	UpdateWorkflow(ctx context.Context, w *SheetWorkflow) error
	DeleteWorkflow(ctx context.Context, id uuid.UUID) error

	// Webhook token rotation. Returns the NEW raw token (caller shows once).
	// Server stores only SHA256(token) hex in triggers_json.
	RotateWebhookToken(ctx context.Context, workflowID uuid.UUID) (rawToken string, err error)

	// Run lifecycle
	CreateRun(ctx context.Context, r *SheetWorkflowRun) error
	GetRun(ctx context.Context, id uuid.UUID) (*SheetWorkflowRun, error)
	ListRunsForWorkflow(ctx context.Context, workflowID uuid.UUID, limit int) ([]SheetWorkflowRun, error)
	UpdateRunProgress(ctx context.Context, runID uuid.UUID, completed, errored int, tokensIn, tokensOut int) error
	FinishRun(ctx context.Context, runID uuid.UUID, status string, errorMessage *string) error

	// Cells — batch write & read for orchestrator + progress UI.
	BulkInitCells(ctx context.Context, runID uuid.UUID, rowCount, colCount int) error
	UpdateCellStatus(ctx context.Context, runID uuid.UUID, rowIdx, colIdx int, status string, errMsg *string, attempt int, tokensIn, tokensOut int, latencyMs *int) error
	ListUnfinishedCells(ctx context.Context, runID uuid.UUID) ([]SheetWorkflowCell, error)

	// Recovery — pick up runs whose goclaw instance crashed mid-flight.
	// Returns runs whose status is queued/running with no recent heartbeat.
	ListRecoverableRuns(ctx context.Context, olderThan time.Duration) ([]SheetWorkflowRun, error)

	// Webhook idempotency
	ClaimIdempotencyKey(ctx context.Context, key string, workflowID uuid.UUID) (alreadySeen bool, priorRunID *uuid.UUID, err error)
	BindIdempotencyRun(ctx context.Context, key string, runID uuid.UUID) error
	SweepExpiredIdempotency(ctx context.Context, now time.Time) (deleted int, err error)
}
