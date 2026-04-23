package pg

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type PGReminderStore struct {
	db *sql.DB
}

func NewPGReminderStore(db *sql.DB) *PGReminderStore {
	return &PGReminderStore{db: db}
}

var _ store.ReminderStore = (*PGReminderStore)(nil)

func (s *PGReminderStore) Insert(ctx context.Context, r *store.Reminder) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.Must(uuid.NewV7())
	}
	if r.DeliveredAt.IsZero() {
		r.DeliveredAt = time.Now().UTC()
	}
	tid := r.TenantID
	if tid == uuid.Nil {
		tid = store.TenantIDFromContext(ctx)
	}
	if tid == uuid.Nil {
		return fmt.Errorf("reminder insert: tenant_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO reminders (id, tenant_id, user_id, job_id, job_name, origin_session_key, channel, content, delivered_at)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9)`,
		r.ID, tid, r.UserID, r.JobID, r.JobName, r.OriginSessionKey, r.Channel, r.Content, r.DeliveredAt,
	)
	if err != nil {
		return fmt.Errorf("reminder insert: %w", err)
	}
	return nil
}

func (s *PGReminderStore) List(ctx context.Context, opts store.ReminderListOpts) ([]store.Reminder, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	q := `SELECT id, tenant_id, user_id, COALESCE(job_id::text, '') as job_id, job_name, origin_session_key, channel, content, delivered_at, read_at
	      FROM reminders WHERE 1=1`
	var args []any
	argIdx := 1

	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return nil, fmt.Errorf("tenant_id required")
		}
		q += fmt.Sprintf(" AND tenant_id = $%d", argIdx)
		args = append(args, tid)
		argIdx++
	}
	if opts.UserID != "" {
		q += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, opts.UserID)
		argIdx++
	}

	q += fmt.Sprintf(" ORDER BY delivered_at DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reminder list: %w", err)
	}
	defer rows.Close()

	var result []store.Reminder
	for rows.Next() {
		var r store.Reminder
		if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.JobID, &r.JobName, &r.OriginSessionKey, &r.Channel, &r.Content, &r.DeliveredAt, &r.ReadAt); err != nil {
			return nil, fmt.Errorf("reminder scan: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *PGReminderStore) MarkRead(ctx context.Context, id uuid.UUID) error {
	q := `UPDATE reminders SET read_at = NOW() WHERE id = $1 AND read_at IS NULL`
	args := []any{id}
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = $2"
		args = append(args, tid)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *PGReminderStore) MarkAllRead(ctx context.Context, userID string) error {
	q := `UPDATE reminders SET read_at = NOW() WHERE read_at IS NULL AND user_id = $1`
	args := []any{userID}
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = $2"
		args = append(args, tid)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *PGReminderStore) Delete(ctx context.Context, id uuid.UUID) error {
	q := `DELETE FROM reminders WHERE id = $1`
	args := []any{id}
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = $2"
		args = append(args, tid)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}
