package upgrade

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// DataHookFunc is a Go function that runs after a specific schema version's
// SQL migration has been applied.
type DataHookFunc func(ctx context.Context, db *sql.DB) error

type dataHook struct {
	SchemaVersion uint
	Name          string
	Fn            DataHookFunc
}

var registry []dataHook

// dataHooksAdvisoryLockID is the PG session-level advisory lock key that
// serializes RunPendingHooks across concurrent gateway processes (prod ASG runs
// >=2 instances against one shared RDS). It mirrors what golang-migrate already
// does for SQL migrations, so enabling GOCLAW_AUTO_UPGRADE is safe on boot.
//
// Key choice: golang-migrate derives its lock id as crc32(dbName)*salt, always
// within the uint32 range (< 2^32). evolutionCronLockID (0x65766F6C) is also
// < 2^32. Any int64 above 2^32 is therefore guaranteed disjoint from both, so a
// collision/deadlock with either is impossible. 0x64617461686F6F6B == "datahook".
const dataHooksAdvisoryLockID int64 = 0x64617461686F6F6B

// RegisterDataHook registers a Go data migration hook for a specific schema version.
// Name must be unique across all hooks. Hooks for the same version run in
// registration order.
func RegisterDataHook(schemaVersion uint, name string, fn DataHookFunc) {
	registry = append(registry, dataHook{
		SchemaVersion: schemaVersion,
		Name:          name,
		Fn:            fn,
	})
}

// PendingHooks returns the names of data hooks that haven't been applied yet.
func PendingHooks(ctx context.Context, db *sql.DB) ([]string, error) {
	if err := ensureDataMigrationsTable(ctx, db); err != nil {
		return nil, err
	}

	applied, err := loadApplied(ctx, db)
	if err != nil {
		return nil, err
	}

	var pending []string
	for _, hook := range registry {
		if !applied[hook.Name] {
			pending = append(pending, hook.Name)
		}
	}
	return pending, nil
}

// RunPendingHooks executes all data hooks that haven't been applied yet.
// Each hook is tracked in the data_migrations table to ensure idempotency.
func RunPendingHooks(ctx context.Context, db *sql.DB) (int, error) {
	if err := ensureDataMigrationsTable(ctx, db); err != nil {
		return 0, fmt.Errorf("ensure data_migrations table: %w", err)
	}

	// Serialize across concurrent gateway processes with a PG advisory lock.
	// The lock and its unlock must run on the SAME session, so pin a dedicated
	// *sql.Conn rather than using the pool (pool calls may land on different
	// connections). Blocking pg_advisory_lock (not pg_try_*) is intentional: a
	// second booting instance must WAIT for the first to finish applying hooks,
	// not skip them and proceed against a half-migrated DB.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("data hooks: pin connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", dataHooksAdvisoryLockID); err != nil {
		return 0, fmt.Errorf("data hooks: acquire advisory lock: %w", err)
	}
	defer func() {
		// Use context.Background(): if ctx is already cancelled we still want to
		// release the lock so other instances are not blocked forever.
		if _, uErr := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", dataHooksAdvisoryLockID); uErr != nil {
			slog.Warn("data hooks: release advisory lock failed", "error", uErr)
		}
	}()

	// Load applied set INSIDE the lock (double-checked). A process that read the
	// set before acquiring the lock could re-run a hook the lock holder just
	// applied; reading here guarantees we see the holder's committed inserts.
	applied, err := loadApplied(ctx, db)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, hook := range registry {
		if applied[hook.Name] {
			continue
		}

		slog.Info("running data migration hook",
			"name", hook.Name,
			"schema_version", hook.SchemaVersion,
		)
		start := time.Now()

		if err := hook.Fn(ctx, db); err != nil {
			return count, fmt.Errorf("data hook %q failed: %w", hook.Name, err)
		}

		// Record completion. ON CONFLICT DO NOTHING is belt-and-suspenders to the
		// advisory lock: the name PRIMARY KEY would otherwise fail a concurrent
		// re-insert, but with the lock held this conflict should never trigger.
		_, err := db.ExecContext(ctx,
			"INSERT INTO data_migrations (name, version, applied_at) VALUES ($1, $2, NOW()) ON CONFLICT (name) DO NOTHING",
			hook.Name, hook.SchemaVersion,
		)
		if err != nil {
			return count, fmt.Errorf("record hook %q: %w", hook.Name, err)
		}

		slog.Info("data migration hook complete",
			"name", hook.Name,
			"duration", time.Since(start),
		)
		count++
	}

	return count, nil
}

func ensureDataMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS data_migrations (
			name       VARCHAR(255) PRIMARY KEY,
			version    INT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	return err
}

func loadApplied(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM data_migrations")
	if err != nil {
		return nil, fmt.Errorf("query data_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}
