package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type SQLiteReminderStore struct {
	db *sql.DB
}

func NewSQLiteReminderStore(db *sql.DB) *SQLiteReminderStore {
	return &SQLiteReminderStore{db: db}
}

var _ store.ReminderStore = (*SQLiteReminderStore)(nil)

func (s *SQLiteReminderStore) Insert(ctx context.Context, r *store.Reminder) error {
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
	var jobID any
	if r.JobID != "" {
		jobID = r.JobID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO reminders (id, tenant_id, user_id, job_id, job_name, origin_session_key, channel, content, delivered_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, tid, r.UserID, jobID, r.JobName, r.OriginSessionKey, r.Channel, r.Content, r.DeliveredAt,
	)
	return err
}

func (s *SQLiteReminderStore) List(ctx context.Context, opts store.ReminderListOpts) ([]store.Reminder, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	q := `SELECT id, tenant_id, user_id, COALESCE(job_id, ''), job_name, origin_session_key, channel, content, delivered_at, read_at
	      FROM reminders WHERE 1=1`
	var args []any
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return nil, fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = ?"
		args = append(args, tid)
	}
	if opts.UserID != "" {
		q += " AND user_id = ?"
		args = append(args, opts.UserID)
	}
	q += " ORDER BY delivered_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []store.Reminder
	for rows.Next() {
		var r store.Reminder
		if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.JobID, &r.JobName, &r.OriginSessionKey, &r.Channel, &r.Content, &r.DeliveredAt, &r.ReadAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *SQLiteReminderStore) MarkRead(ctx context.Context, id uuid.UUID) error {
	q := `UPDATE reminders SET read_at = ? WHERE id = ? AND read_at IS NULL`
	args := []any{time.Now().UTC(), id}
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = ?"
		args = append(args, tid)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *SQLiteReminderStore) MarkAllRead(ctx context.Context, userID string) error {
	q := `UPDATE reminders SET read_at = ? WHERE read_at IS NULL AND user_id = ?`
	args := []any{time.Now().UTC(), userID}
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = ?"
		args = append(args, tid)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *SQLiteReminderStore) Delete(ctx context.Context, id uuid.UUID) error {
	q := `DELETE FROM reminders WHERE id = ?`
	args := []any{id}
	if !store.IsCrossTenant(ctx) {
		tid := store.TenantIDFromContext(ctx)
		if tid == uuid.Nil {
			return fmt.Errorf("tenant_id required")
		}
		q += " AND tenant_id = ?"
		args = append(args, tid)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}
