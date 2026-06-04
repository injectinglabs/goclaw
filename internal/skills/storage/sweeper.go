package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/diskutil"
)

// SweeperConfig drives the local skill-cache cleanup loop. With S3 as
// the durable source of truth, local disk is just a read-through cache:
// evicting an old version costs at most one S3 GET the next time the
// agent touches it (via lazy-fetch in the install / read path).
//
// We only ever evict skill versions whose DB row is `archived` —
// `active` rows stay because they're the live version the agent is
// expected to read RIGHT NOW; one S3 RTT on a hot path is the kind of
// latency spike that makes users complain.
type SweeperConfig struct {
	// Interval between sweeps. 0 disables the sweeper.
	Interval time.Duration
	// DiskLimitPercent triggers eviction. 0 disables the pressure check.
	// 70 is a sensible default — matches the media sweeper.
	DiskLimitPercent int
}

// Sweeper holds the dependencies a sweep cycle needs. Kept tiny on
// purpose so a future test can swap in a stub DB / dataDir.
type Sweeper struct {
	mirror  *Mirror
	db      *sql.DB
	dataDir string
}

// NewSweeper wires the dependencies. Returns nil when mirror or db is
// missing — there's nothing to evict in lite/desktop editions without
// a remote backing store to recover from.
func NewSweeper(mirror *Mirror, db *sql.DB, dataDir string) *Sweeper {
	if mirror == nil || db == nil || dataDir == "" {
		return nil
	}
	return &Sweeper{mirror: mirror, db: db, dataDir: dataDir}
}

// Start launches the background goroutine. Runs once immediately so a
// fresh boot can shed any local versions that already crossed the line,
// then on every tick.
func (s *Sweeper) Start(ctx context.Context, cfg SweeperConfig) {
	if s == nil || cfg.Interval == 0 {
		if s != nil {
			slog.Info("skills.cache.sweeper_disabled", "reason", "interval=0")
		}
		return
	}
	go func() {
		s.SweepOnce(ctx, cfg)
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.SweepOnce(ctx, cfg)
			}
		}
	}()
	slog.Info("skills.cache.sweeper_started",
		"interval", cfg.Interval, "disk_limit_pct", cfg.DiskLimitPercent)
}

// SweepOnce runs a single disk-pressure cleanup pass over archived
// skill versions. Errors are logged; the goroutine never returns an
// error because there's no useful recovery beyond the next tick.
func (s *Sweeper) SweepOnce(ctx context.Context, cfg SweeperConfig) {
	if cfg.DiskLimitPercent <= 0 || cfg.DiskLimitPercent >= 100 {
		return
	}
	start := time.Now()

	usageBefore, err := diskutil.Fraction(s.dataDir)
	if err != nil {
		slog.Warn("skills.cache.statfs_failed", "error", err)
		return
	}
	limit := float64(cfg.DiskLimitPercent) / 100.0
	if usageBefore < limit {
		return // not under pressure
	}
	// 5-point hysteresis so we don't flap right after eviction.
	target := limit - 0.05
	if target < 0.05 {
		target = 0.05
	}

	victims, err := s.archivedVersionsOldestFirst(ctx)
	if err != nil {
		slog.Warn("skills.cache.candidates_failed", "error", err)
		return
	}

	var evicted int
	var bytes int64
	for _, v := range victims {
		if ctx.Err() != nil {
			break
		}
		size, err := dirSize(v.localDir)
		if err != nil {
			continue // already gone
		}
		if rmErr := os.RemoveAll(v.localDir); rmErr != nil {
			slog.Warn("skills.cache.remove_failed",
				"slug", v.slug, "version", v.version, "error", rmErr)
			continue
		}
		evicted++
		bytes += size
		// Re-check usage every 20 evictions. dirSize is cheap; statfs is
		// cheap too but no need to call it per skill.
		if evicted%20 == 0 {
			if u, uerr := diskutil.Fraction(s.dataDir); uerr == nil && u < target {
				break
			}
		}
	}

	usageAfter, _ := diskutil.Fraction(s.dataDir)
	slog.Info("skills.cache.sweep",
		"evicted", evicted, "bytes", bytes,
		"usage_before_pct", roundPct(usageBefore),
		"usage_after_pct", roundPct(usageAfter),
		"elapsed_ms", time.Since(start).Milliseconds())
}

// archivedCandidate is one DB row eligible for eviction.
type archivedCandidate struct {
	slug     string
	version  int
	localDir string
}

// archivedVersionsOldestFirst lists every archived skill row with
// an S3-backed source so we know we can rehydrate from the mirror.
// Active versions are intentionally excluded — they're the hot path.
func (s *Sweeper) archivedVersionsOldestFirst(ctx context.Context) ([]archivedCandidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.slug, s.version, s.tenant_id, t.slug AS tenant_slug
		   FROM skills s
		   JOIN tenants t ON t.id = s.tenant_id
		  WHERE s.status = 'archived'
		    AND s.source_url IS NOT NULL
		    AND s.tenant_id IS NOT NULL
		  ORDER BY s.updated_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("skills.cache: query archived: %w", err)
	}
	defer rows.Close()
	var out []archivedCandidate
	for rows.Next() {
		var (
			slug, tenantSlug string
			version          int
			tenantID         uuid.UUID
		)
		if err := rows.Scan(&slug, &version, &tenantID, &tenantSlug); err != nil {
			continue
		}
		dir := filepath.Join(
			config.TenantSkillsStoreDir(s.dataDir, tenantID, tenantSlug),
			slug, fmt.Sprintf("%d", version),
		)
		out = append(out, archivedCandidate{slug: slug, version: version, localDir: dir})
	}
	return out, rows.Err()
}

// dirSize sums the size of every regular file under dir. Symlinks are
// not followed — they don't count toward our footprint anyway.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// roundPct trims a 0..1 ratio to one-decimal-place percent, e.g.
// 0.7234 -> 72.3.
func roundPct(r float64) float64 {
	v := r * 100
	return float64(int64(v*10+0.5)) / 10
}

