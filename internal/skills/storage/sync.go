package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/config"

	"github.com/google/uuid"
)

// SyncResult is the aggregate outcome of a SyncFromS3 sweep, returned
// for logging at boot and exposed via /health for ops dashboards.
type SyncResult struct {
	Checked    int           // total DB rows iterated
	Downloaded int           // tenant skills hydrated from S3
	AlreadyOK  int           // local dir was already populated
	Failed     int           // S3 errors / missing prefixes
	Elapsed    time.Duration // wall time of the sweep
}

// SyncFromS3 runs at gateway startup to make sure every tenant skill
// registered in DB has its files present locally. Without this, an ASG
// node that joined after a skill was installed on a sibling would see
// the DB row but no on-disk content, and read_file would 404.
//
// Scope:
//   - Iterates `skills` rows where tenant_id IS NOT NULL (system skills
//     are seeded from /app/bundled-skills, not S3).
//   - Skips rows whose local dir already has files (idempotent — fine
//     to run on every boot).
//   - Best-effort per row: a failure on one skill logs and continues so
//     a stuck S3 doesn't keep the whole node down.
//
// dataDir is the same path the install handler uses; we recompute the
// per-tenant skills-store dir with config.TenantSkillsStoreDir to keep
// the layout convention in one place.
func SyncFromS3(ctx context.Context, db *sql.DB, m *Mirror, dataDir string) (SyncResult, error) {
	if m == nil || db == nil {
		return SyncResult{}, nil
	}
	start := time.Now()
	rows, err := db.QueryContext(ctx,
		`SELECT s.id, s.slug, s.version, s.tenant_id, t.slug
		   FROM skills s
		   JOIN tenants t ON t.id = s.tenant_id
		  WHERE s.status != 'deleted'
		    AND s.source_url IS NOT NULL
		    AND s.tenant_id IS NOT NULL`)
	if err != nil {
		return SyncResult{}, fmt.Errorf("skills.s3.sync: query: %w", err)
	}
	defer rows.Close()

	var res SyncResult
	for rows.Next() {
		var (
			id         uuid.UUID
			slug       string
			version    int
			tenantID   uuid.UUID
			tenantSlug string
		)
		if err := rows.Scan(&id, &slug, &version, &tenantID, &tenantSlug); err != nil {
			slog.Warn("skills.s3.sync.scan_failed", "error", err)
			continue
		}
		res.Checked++

		localDir := filepath.Join(
			config.TenantSkillsStoreDir(dataDir, tenantID, tenantSlug),
			slug, fmt.Sprintf("%d", version),
		)
		keyPrefix := m.SkillKeyPrefix(tenantSlug, slug, version)

		downloaded, err := m.EnsureLocal(ctx, keyPrefix, localDir)
		switch {
		case err != nil:
			res.Failed++
			slog.Warn("skills.s3.sync.failed",
				"slug", slug, "version", version, "tenant", tenantSlug, "error", err)
		case downloaded == 0:
			res.AlreadyOK++
		default:
			res.Downloaded++
			slog.Info("skills.s3.sync.hydrated",
				"slug", slug, "version", version, "tenant", tenantSlug, "files", downloaded)
		}
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("skills.s3.sync: iter: %w", err)
	}
	res.Elapsed = time.Since(start)
	slog.Info("skills.s3.sync.done",
		"checked", res.Checked, "downloaded", res.Downloaded,
		"already_ok", res.AlreadyOK, "failed", res.Failed,
		"elapsed_ms", res.Elapsed.Milliseconds())
	return res, nil
}
