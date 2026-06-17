# Sheet Workflows — WS event schema + rehydration contract

This document is the source-of-truth for the goclaw ↔ SPA contract used by
the Paradigm-style split-view sheet canvas (Phase A, task #186). It locks
down:

1. The `workflow.event` payload that the orchestrator publishes to the WS
   bus per cell-state change.
2. The forthcoming `workflow.runState` JSON-RPC method the SPA calls on
   reconnect to rehydrate full per-cell state without polling.

Any change to either contract here MUST be made in lockstep on both sides
(Go struct in `internal/workflow/runtime/runtime.go` + TS schema in
`injecting-ai-web-agent-website/src/sheetWorkflow/events.ts`).

---

## 1. `workflow.event` WS payload

Emitted by `runtime.Orchestrator` via the `EventBus` interface
(`PublishWorkflowEvent`). The concrete bus impl is `BusEventBus` in
`internal/workflow/runtime/eventbus_adapter.go`, which forwards onto
the gateway's tenant-scoped event publisher under the fixed event name
`"workflow.event"`. Per-tenant filtering is handled by the existing
gateway event filter — clients only see events for their own tenant.

### Go struct (authoritative)

```go
// internal/workflow/runtime/runtime.go
type RunEvent struct {
    Type       string    `json:"type"`         // see "Event types" below
    RunID      uuid.UUID `json:"run_id"`
    WorkflowID uuid.UUID `json:"workflow_id"`
    TenantID   uuid.UUID `json:"tenant_id"`
    UserID     string    `json:"user_id"`

    // cell.update fields (nil for non-cell events)
    RowIdx     *int    `json:"row_idx,omitempty"`
    ColIdx     *int    `json:"col_idx,omitempty"`
    CellStatus *string `json:"cell_status,omitempty"` // queued|running|done|error|skipped
    CellError  *string `json:"cell_error,omitempty"`

    // run-level fields (set on run.started / progress / completed / error)
    Completed int    `json:"completed,omitempty"`
    Errored   int    `json:"errored,omitempty"`
    Total     int    `json:"total,omitempty"`
    Status    string `json:"status,omitempty"`  // queued|running|done|cancelled|error
    Message   string `json:"message,omitempty"`
}
```

### TS schema (mirror — must stay in sync)

```ts
// src/sheetWorkflow/events.ts
export const SheetCellStatus = z.enum(['queued', 'running', 'done', 'error', 'skipped'])
export const RunStatus       = z.enum(['queued', 'running', 'done', 'cancelled', 'error'])
export const EventType       = z.enum([
  'run.started', 'cell.update', 'run.progress', 'run.completed', 'run.error',
])

export const SheetRunEvent = z.object({
  type:         EventType,
  run_id:       z.string().uuid(),
  workflow_id:  z.string().uuid(),
  tenant_id:    z.string().uuid().optional(),
  user_id:      z.string().optional(),

  row_idx:      z.number().int().nonnegative().optional(),
  col_idx:      z.number().int().nonnegative().optional(),
  cell_status:  SheetCellStatus.optional(),
  cell_error:   z.string().optional(),

  completed:    z.number().int().nonnegative().optional(),
  errored:      z.number().int().nonnegative().optional(),
  total:        z.number().int().nonnegative().optional(),
  status:       RunStatus.optional(),
  message:      z.string().optional(),
})
```

Validate **at the WS-message boundary** in the SPA — never trust the wire
payload to the reducer directly. A malformed event is dropped + logged;
the reducer only ever receives a parsed, validated `SheetRunEvent`.

### Event types

| `type` | Fields set | When |
|---|---|---|
| `run.started` | `total` (and run-level meta) | After orchestrator initialises `sheet_workflow_cells` rows, before first wave fans out. |
| `cell.update` | `row_idx`, `col_idx`, `cell_status`, optional `cell_error` | Each per-cell state transition (queued → running → done/error). One event per change. |
| `run.progress` | `completed`, `errored`, `total` | Emitted on wave-flush boundaries so clients can update the counter without summing cells. |
| `run.completed` | `status="done"`, `completed`, `errored`, `total` | Run finished successfully. |
| `run.error` | `status="error"`, `message` | Run aborted (auth, no rows, sheet inaccessible, orchestrator panic). |

### Ordering & at-least-once semantics

- Events for a given `run_id` are emitted in chronological order **per cell**.
- Cross-cell order is **not** guaranteed (waves run in parallel; one cell's
  `done` may overtake another cell's `running`).
- The bus is at-least-once: clients may see duplicates after a reconnect
  before the orchestrator's resubscribe completes.
- → SPA reducer MUST be **idempotent**: re-applying the same event yields
  the same state. Status transitions follow a monotone lattice
  (`queued < running < done|error|skipped`); a later message that would
  regress status is ignored.

### Rate

Typical 20-cell run: ~40 cell events + 3 run-level. Heavy runs (200 rows ×
6 cols = 1200 cells): ~2400 cell events over 30-90 seconds. SPA-side
batching (`requestAnimationFrame`-flush in the Zustand store) is required
to keep the grid render budget under 16 ms / frame.

---

## 2. `workflow.runState` JSON-RPC (forthcoming)

Server method exposed over the same WS connection the SPA uses for chat.
Mirrors `chat.activeSessions` pattern (see fork PR for that method) —
client-facing, cognito-auth via the existing WS session, no new HTTP
surface, no new bearer token.

### Why this exists

`workflow.event` is at-least-once but **not durable** — events fired
before the SPA reconnected are gone. To support page reload mid-run and
network blips, the SPA queries this method on reconnect for each active
`runId` it knows about (from IndexedDB snapshot) and folds the returned
snapshot into the store before resuming live event consumption.

### Request

```json
{
  "method": "workflow.runState",
  "params": { "run_id": "<uuid>" }
}
```

### Response

```ts
{
  run: {
    id: string,                 // run UUID
    workflow_id: string,
    status: 'queued' | 'running' | 'done' | 'cancelled' | 'error',
    row_count: number,
    completed_count: number,
    error_count: number,
    error_message?: string,
    started_at?: string,        // RFC3339
    finished_at?: string,
  },
  cells: Array<{
    row_idx: number,
    col_idx: number,
    status: 'queued' | 'running' | 'done' | 'error' | 'skipped',
    error_message?: string,
    attempt: number,
    tokens_in: number,
    tokens_out: number,
    latency_ms?: number,
  }>,
  // Column schema is part of the workflow definition, not the run state.
  // The SPA loads it from the same `workflow.get(workflow_id)` method
  // it uses elsewhere (or pulls it from the original chat tool result).
}
```

### Auth

- Same WS session = same `(tenant_id, user_id)` as the chat connection.
- Server checks `run.tenant_id == session.tenant_id`. Mismatch → 403.
- No "by org" semantics here: a run belongs to its creator's tenant only.

### Errors

| Code | Cause |
|---|---|
| `not_found` | `run_id` doesn't exist or belongs to another tenant |
| `internal` | DB unreachable |

### Implementation notes

Server: new `gateway/handlers/workflow_runstate.go` reading from
`store.SheetWorkflowStore.GetRun` + a new `ListAllCells(runID)` method on
the store (the existing `ListUnfinishedCells` returns only queued/running
— rehydration needs the full set including `done`/`error`).

---

## 3. SPA reducer rules (informative)

Mirrored verbatim from `feat/sheet-message-bubble` branch
(`sheetBubble.ts`), with the multi-run, OOO-safe, persist-friendly
extensions for Phase A.

1. **`reduceRun(state, event)`** is pure — same `(state, event)` → same
   output. No `Date.now()` reads. No I/O. Easy to test.
2. **Idempotent**: applying an event twice yields the same state as
   applying once.
3. **Monotone status**: `queued → running → done|error|skipped`.
   A backward transition is silently dropped (no warn — it's a normal
   consequence of OOO bus delivery).
4. **Multi-run**: store is `Map<runId, RunSliceState>`. Event for an
   unknown `runId` triggers an automatic `initRunSlice` from the event's
   `workflow_id` (so a fresh tab catching mid-flight events still works).
5. **Persistence**: every state mutation triggers a debounced (200 ms)
   IndexedDB write via `zustand/persist` middleware. On boot, slices are
   restored synchronously so the canvas can render previous progress
   before the WS reconnects.
6. **Reconnect resync**: on WS open, for each `runId` in the store with
   `status ∈ {queued, running}`, fire one `workflow.runState` and fold
   the returned snapshot via `mergeSnapshot(state, snapshot)`. Live
   events that arrived during the merge are queued and applied after.

---

## 4. Stability & versioning

The Go `RunEvent` struct and the JSON-RPC response shape are versioned by
**additive evolution only**. Renames, type changes, or field removal
require coordinated SPA + server release. Adding optional fields is
always safe — TS schema uses `.optional()` for everything beyond the
`type` discriminator.

Never collapse the `run.progress` event into `cell.update` — the SPA
relies on `progress` events to fast-update the counter without summing
the (potentially 1000+) cell map.

---

## 5. References

- Go: `internal/workflow/runtime/runtime.go` (RunEvent, Orchestrator, emit)
- Go: `internal/workflow/runtime/eventbus_adapter.go` (bus glue)
- Go: `internal/store/sheet_workflow_store.go` (SheetWorkflowRun, Cell)
- TS (existing): `src/sheetBubble.ts` on `feat/sheet-message-bubble`
- TS (planned): `src/sheetWorkflow/{store,events,persist,api}.ts`
- Task tracker: #186 (parent), #189 (this PR), #190-195 (frontend work)
