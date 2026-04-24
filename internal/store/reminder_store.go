package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Reminder is a durable record of a cron job delivery to an internal-channel
// user (ws/browser). Intentionally NOT foreign-keyed to cron_jobs — one-shot
// jobs get auto-deleted after firing but the reminder row must survive.
type Reminder struct {
	ID               uuid.UUID  `json:"id" db:"id"`
	TenantID         uuid.UUID  `json:"tenant_id" db:"tenant_id"`
	UserID           string     `json:"user_id" db:"user_id"`
	JobID            string     `json:"job_id,omitempty" db:"job_id"`
	JobName          string     `json:"job_name,omitempty" db:"job_name"`
	OriginSessionKey string     `json:"origin_session_key" db:"origin_session_key"`
	Channel          string     `json:"channel" db:"channel"`
	Content          string     `json:"content" db:"content"`
	DeliveredAt      time.Time  `json:"delivered_at" db:"delivered_at"`
	ReadAt           *time.Time `json:"read_at,omitempty" db:"read_at"`
}

// ReminderListOpts configures reminder listing.
type ReminderListOpts struct {
	UserID string // filter by owner (optional; empty = all tenant users)
	Limit  int    // default 100
	Offset int
}

// ReminderStore manages the reminder inbox backing the extension UI.
type ReminderStore interface {
	Insert(ctx context.Context, r *Reminder) error
	List(ctx context.Context, opts ReminderListOpts) ([]Reminder, error)
	MarkRead(ctx context.Context, id uuid.UUID) error
	MarkAllRead(ctx context.Context, userID string) error
	Delete(ctx context.Context, id uuid.UUID) error
}
