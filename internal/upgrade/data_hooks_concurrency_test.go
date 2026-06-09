//go:build integration

package upgrade

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// defaultTestDSN matches the pgvector test container in the README.
const defaultTestDSN = "postgres://postgres:test@localhost:5433/goclaw_test?sslmode=disable"

// TestRunPendingHooks_ConcurrentSingleApply proves the advisory lock makes
// RunPendingHooks safe under the prod topology (>=2 gateway processes booting
// against one shared RDS). Without the lock, multiple callers each see the hook
// as pending and run its Fn (data double-apply), and the losers fail on the
// data_migrations name PRIMARY KEY. With the lock, the Fn runs exactly once,
// every caller succeeds, and exactly one row is recorded.
func TestRunPendingHooks_ConcurrentSingleApply(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("test PG not available: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test PG not available: %v", err)
	}

	// Isolate the global registry to a single probe hook; restore afterwards so
	// we don't leak it into any other test in this package.
	savedRegistry := registry
	defer func() { registry = savedRegistry }()

	const hookName = "test_concurrent_apply_probe"
	if err := ensureDataMigrationsTable(ctx, db); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM data_migrations WHERE name = $1", hookName); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM data_migrations WHERE name = $1", hookName)
	})

	var fnRuns int32
	registry = []dataHook{{
		SchemaVersion: 999999,
		Name:          hookName,
		Fn: func(_ context.Context, _ *sql.DB) error {
			atomic.AddInt32(&fnRuns, 1)
			// Widen the race window: hold the lock long enough that the other
			// callers are guaranteed to be waiting on pg_advisory_lock.
			time.Sleep(100 * time.Millisecond)
			return nil
		},
	}}

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	counts := make([]int, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			counts[i], errs[i] = RunPendingHooks(ctx, db)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("RunPendingHooks caller %d errored: %v", i, e)
		}
	}
	if got := atomic.LoadInt32(&fnRuns); got != 1 {
		t.Fatalf("hook Fn ran %d times, want exactly 1", got)
	}

	totalApplied := 0
	for _, c := range counts {
		totalApplied += c
	}
	if totalApplied != 1 {
		t.Fatalf("callers reported %d applies in total, want 1", totalApplied)
	}

	var rows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM data_migrations WHERE name = $1", hookName).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("data_migrations has %d rows for probe hook, want 1", rows)
	}
}
