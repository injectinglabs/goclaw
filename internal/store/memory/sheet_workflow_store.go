// Package memory provides in-memory implementations of store interfaces
// for tests and local development. Not safe for production — no
// persistence, no concurrency safety beyond a sync.Mutex.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SheetWorkflowStore is a thread-safe in-memory impl of
// store.SheetWorkflowStore, used by orchestrator integration tests.
type SheetWorkflowStore struct {
	mu sync.RWMutex

	workflows map[uuid.UUID]*store.SheetWorkflow
	runs      map[uuid.UUID]*store.SheetWorkflowRun
	cells     map[uuid.UUID]map[[2]int]*store.SheetWorkflowCell // run_id → (row,col) → cell
	idemp     map[string]*store.WebhookIdempotencyKey

	// Hook so tests can observe persisted updates without polling.
	OnUpdateRun  func(*store.SheetWorkflowRun)
	OnUpdateCell func(*store.SheetWorkflowCell)
}

func NewSheetWorkflowStore() *SheetWorkflowStore {
	return &SheetWorkflowStore{
		workflows: make(map[uuid.UUID]*store.SheetWorkflow),
		runs:      make(map[uuid.UUID]*store.SheetWorkflowRun),
		cells:     make(map[uuid.UUID]map[[2]int]*store.SheetWorkflowCell),
		idemp:     make(map[string]*store.WebhookIdempotencyKey),
	}
}

// ─── Workflows ─────────────────────────────────────────────────────

func (s *SheetWorkflowStore) CreateWorkflow(_ context.Context, w *store.SheetWorkflow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	if w.Status == "" {
		w.Status = "active"
	}
	if w.Visibility == "" {
		w.Visibility = "personal"
	}
	now := time.Now()
	w.CreatedAt, w.UpdatedAt = now, now
	cp := *w
	s.workflows[w.ID] = &cp
	return nil
}

func (s *SheetWorkflowStore) GetWorkflow(_ context.Context, id uuid.UUID) (*store.SheetWorkflow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.workflows[id]
	if !ok {
		return nil, nil
	}
	cp := *w
	return &cp, nil
}

func (s *SheetWorkflowStore) ListWorkflowsForUser(_ context.Context, tenantID uuid.UUID, userID, role string) ([]store.SheetWorkflow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.SheetWorkflow
	for _, w := range s.workflows {
		if w.TenantID != tenantID {
			continue
		}
		if role != "owner" && role != "admin" {
			if w.UserID != userID && w.Visibility != "team" {
				continue
			}
		}
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (s *SheetWorkflowStore) UpdateWorkflow(_ context.Context, w *store.SheetWorkflow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.workflows[w.ID]
	if !ok {
		return errors.New("workflow not found")
	}
	w.UpdatedAt = time.Now()
	w.CreatedAt = existing.CreatedAt
	cp := *w
	s.workflows[w.ID] = &cp
	return nil
}

func (s *SheetWorkflowStore) DeleteWorkflow(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workflows, id)
	return nil
}

func (s *SheetWorkflowStore) RotateWebhookToken(_ context.Context, workflowID uuid.UUID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workflows[workflowID]
	if !ok {
		return "", errors.New("workflow not found")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	rawHex := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(rawHex))
	tokenHash := hex.EncodeToString(sum[:])

	updated := false
	for i := range w.Triggers {
		if w.Triggers[i].Type == "webhook" {
			w.Triggers[i].TokenHash = tokenHash
			updated = true
			break
		}
	}
	if !updated {
		w.Triggers = append(w.Triggers, store.SheetWorkflowTrigger{
			Type:      "webhook",
			TokenHash: tokenHash,
		})
	}
	w.UpdatedAt = time.Now()
	return rawHex, nil
}

// ─── Runs ──────────────────────────────────────────────────────────

func (s *SheetWorkflowStore) CreateRun(_ context.Context, r *store.SheetWorkflowRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.Status == "" {
		r.Status = "queued"
	}
	now := time.Now()
	r.CreatedAt, r.UpdatedAt = now, now
	cp := *r
	s.runs[r.ID] = &cp
	return nil
}

func (s *SheetWorkflowStore) GetRun(_ context.Context, id uuid.UUID) (*store.SheetWorkflowRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (s *SheetWorkflowStore) ListRunsForWorkflow(_ context.Context, workflowID uuid.UUID, limit int) ([]store.SheetWorkflowRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 20
	}
	var out []store.SheetWorkflowRun
	for _, r := range s.runs {
		if r.WorkflowID == workflowID {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *SheetWorkflowStore) UpdateRunProgress(_ context.Context, runID uuid.UUID, completed, errored, tokensIn, tokensOut int) error {
	s.mu.Lock()
	r, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		return errors.New("run not found")
	}
	r.CompletedCount = completed
	r.ErrorCount = errored
	r.TotalTokensIn = tokensIn
	r.TotalTokensOut = tokensOut
	if r.Status == "queued" {
		r.Status = "running"
	}
	if r.StartedAt == nil {
		t := time.Now()
		r.StartedAt = &t
	}
	r.UpdatedAt = time.Now()
	cb := s.OnUpdateRun
	cp := *r
	s.mu.Unlock()
	if cb != nil {
		cb(&cp)
	}
	return nil
}

func (s *SheetWorkflowStore) FinishRun(_ context.Context, runID uuid.UUID, status string, errMsg *string) error {
	s.mu.Lock()
	r, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		return errors.New("run not found")
	}
	r.Status = status
	r.ErrorMessage = errMsg
	t := time.Now()
	r.FinishedAt = &t
	r.UpdatedAt = t
	if w, ok := s.workflows[r.WorkflowID]; ok {
		w.LastRunAt = &t
		w.UpdatedAt = t
	}
	cb := s.OnUpdateRun
	cp := *r
	s.mu.Unlock()
	if cb != nil {
		cb(&cp)
	}
	return nil
}

// ─── Cells ─────────────────────────────────────────────────────────

func (s *SheetWorkflowStore) BulkInitCells(_ context.Context, runID uuid.UUID, rowCount, colCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cells[runID] == nil {
		s.cells[runID] = make(map[[2]int]*store.SheetWorkflowCell)
	}
	for r := 0; r < rowCount; r++ {
		for c := 0; c < colCount; c++ {
			s.cells[runID][[2]int{r, c}] = &store.SheetWorkflowCell{
				RunID:     runID,
				RowIdx:    r,
				ColIdx:    c,
				Status:    "queued",
				UpdatedAt: time.Now(),
			}
		}
	}
	if r, ok := s.runs[runID]; ok {
		r.RowCount = rowCount
	}
	return nil
}

func (s *SheetWorkflowStore) UpdateCellStatus(_ context.Context, runID uuid.UUID, rowIdx, colIdx int, status string, errMsg *string, attempt, tokensIn, tokensOut int, latencyMs *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.cells[runID]
	if !ok {
		return errors.New("run has no cells")
	}
	c, ok := bucket[[2]int{rowIdx, colIdx}]
	if !ok {
		return errors.New("cell not found")
	}
	c.Status = status
	c.ErrorMessage = errMsg
	c.Attempt = attempt
	c.TokensIn = tokensIn
	c.TokensOut = tokensOut
	c.LatencyMs = latencyMs
	c.UpdatedAt = time.Now()
	cb := s.OnUpdateCell
	cp := *c
	if cb != nil {
		// callback outside lock would be cleaner but we keep test impl simple
		defer cb(&cp)
	}
	return nil
}

func (s *SheetWorkflowStore) ListUnfinishedCells(_ context.Context, runID uuid.UUID) ([]store.SheetWorkflowCell, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket, ok := s.cells[runID]
	if !ok {
		return nil, nil
	}
	var out []store.SheetWorkflowCell
	for _, c := range bucket {
		if c.Status == "queued" || c.Status == "running" {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *SheetWorkflowStore) ListRecoverableRuns(_ context.Context, olderThan time.Duration) ([]store.SheetWorkflowRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().Add(-olderThan)
	var out []store.SheetWorkflowRun
	for _, r := range s.runs {
		if (r.Status == "queued" || r.Status == "running") && r.UpdatedAt.Before(cutoff) {
			out = append(out, *r)
		}
	}
	return out, nil
}

// ─── Idempotency ───────────────────────────────────────────────────

func (s *SheetWorkflowStore) ClaimIdempotencyKey(_ context.Context, key string, workflowID uuid.UUID) (bool, *uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.idemp[key]; ok {
		return true, k.RunID, nil
	}
	s.idemp[key] = &store.WebhookIdempotencyKey{
		Key:        key,
		WorkflowID: workflowID,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	}
	return false, nil, nil
}

func (s *SheetWorkflowStore) BindIdempotencyRun(_ context.Context, key string, runID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.idemp[key]
	if !ok {
		return errors.New("idempotency key not claimed")
	}
	id := runID
	k.RunID = &id
	return nil
}

func (s *SheetWorkflowStore) SweepExpiredIdempotency(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.idemp {
		if v.ExpiresAt.Before(now) {
			delete(s.idemp, k)
			n++
		}
	}
	return n, nil
}

// Diagnostic helpers for tests.

func (s *SheetWorkflowStore) Snapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type snap struct {
		Workflows int `json:"workflows"`
		Runs      int `json:"runs"`
		Cells     int `json:"cells"`
	}
	cells := 0
	for _, b := range s.cells {
		cells += len(b)
	}
	b, _ := json.Marshal(snap{len(s.workflows), len(s.runs), cells})
	return string(b)
}
