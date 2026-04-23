package pg

import (
	"time"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// cronRunLogRow is an sqlx scan struct for cron_run_logs SELECT queries.
// All store.CronRunLogEntry fields are db:"-" so a dedicated row struct is required.
type cronRunLogRow struct {
	// JobID may be NULL once the parent cron_jobs row is deleted (one-shot
	// completion) — see migration 000057. We still emit jobId as a string
	// downstream, falling back to "" when null.
	JobID            *uuid.UUID `db:"job_id"`
	JobName          string     `db:"job_name"`
	OriginSessionKey string     `db:"origin_session_key"`
	Status           string     `db:"status"`
	Error            *string    `db:"error"`
	Summary          *string    `db:"summary"`
	RanAt            time.Time  `db:"ran_at"`
	DurationMS       int64      `db:"duration_ms"`
	InputTokens      int        `db:"input_tokens"`
	OutputTokens     int        `db:"output_tokens"`
}

// toCronRunLogEntry converts a cronRunLogRow to store.CronRunLogEntry.
func (r *cronRunLogRow) toCronRunLogEntry() store.CronRunLogEntry {
	var jobID string
	if r.JobID != nil {
		jobID = r.JobID.String()
	}
	return store.CronRunLogEntry{
		Ts:               r.RanAt.UnixMilli(),
		JobID:            jobID,
		JobName:          r.JobName,
		OriginSessionKey: r.OriginSessionKey,
		Status:           r.Status,
		Error:            derefStr(r.Error),
		Summary:          derefStr(r.Summary),
		DurationMS:       r.DurationMS,
		InputTokens:      r.InputTokens,
		OutputTokens:     r.OutputTokens,
	}
}
