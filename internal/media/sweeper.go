package media

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SweeperConfig controls the local media-cache cleanup loop.
//
// Two policies run on every tick:
//
//  1. TTL eviction — anything in .media-cache/ whose mtime is older than
//     TTL is removed. Keep TTL aligned with (or slightly under) the S3
//     bucket lifecycle so we never hold cache entries that point at
//     objects S3 has already lifecycled away.
//
//  2. Disk-pressure LRU — when the filesystem holding the cache crosses
//     DiskLimitPercent (e.g. 70%), the sweeper deletes the oldest files
//     by mtime until usage falls below `DiskLimitPercent - 5%` (built-in
//     5-point hysteresis to avoid flapping when traffic refills the
//     cache right after eviction).
//
// Disk pressure is measured for the whole filesystem hosting the cache
// (via statfs), not the cache directory alone. That's the metric that
// actually matters — if the EBS volume is full goclaw can't write at
// all, regardless of who's using the bytes.
//
// Eviction is always safe: FilesHandler re-hydrates from S3 via
// mediastore.ResolveLocalPath on a cache miss (see internal/http/files.go).
// Deleting a hot file just costs an extra S3 GET on the next request,
// not user-visible data loss.
type SweeperConfig struct {
	// Interval between sweeps. 0 disables the sweeper entirely.
	Interval time.Duration
	// TTL after which a cached file is unconditionally deleted, even if
	// disk pressure is fine. Should match or slightly precede the S3
	// bucket lifecycle (30 days on prod, 7 days on stage).
	TTL time.Duration
	// DiskLimitPercent triggers LRU eviction. 0 disables the pressure
	// check (TTL still runs).
	DiskLimitPercent int
}

// StartSweeper launches the cache-cleanup goroutine. It runs once
// immediately (so a fresh boot picks up files left by the previous
// process) and then on every tick. Returns immediately; the goroutine
// stops when ctx is cancelled.
func (b *S3Backend) StartSweeper(ctx context.Context, cfg SweeperConfig) {
	if cfg.Interval == 0 {
		slog.Info("media.cache.sweeper_disabled", "reason", "interval=0")
		return
	}
	go func() {
		// One pre-tick run so restarts catch up immediately.
		b.SweepOnce(ctx, cfg)
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.SweepOnce(ctx, cfg)
			}
		}
	}()
	slog.Info("media.cache.sweeper_started",
		"interval", cfg.Interval, "ttl", cfg.TTL, "disk_limit_pct", cfg.DiskLimitPercent,
		"cache_dir", b.cacheDir)
}

// SweepOnce runs a single cleanup pass: TTL eviction, then disk-pressure
// LRU. Errors are logged but never returned because the caller is the
// goroutine — there's no useful recovery, only logging.
func (b *S3Backend) SweepOnce(ctx context.Context, cfg SweeperConfig) {
	start := time.Now()

	ttlCount, ttlBytes := b.evictByTTL(ctx, cfg.TTL)
	lruCount, lruBytes, usageBefore, usageAfter := b.evictByDiskPressure(ctx, cfg.DiskLimitPercent)

	b.removeEmptyDirs()

	slog.Info("media.cache.sweep",
		"ttl_deleted", ttlCount, "ttl_bytes", ttlBytes,
		"lru_deleted", lruCount, "lru_bytes", lruBytes,
		"usage_before_pct", roundFloat(usageBefore*100, 1),
		"usage_after_pct", roundFloat(usageAfter*100, 1),
		"elapsed_ms", time.Since(start).Milliseconds())
}

// evictByTTL deletes cache files whose mtime is older than now - ttl.
// When ttl is zero the phase is skipped.
func (b *S3Backend) evictByTTL(ctx context.Context, ttl time.Duration) (int, int64) {
	if ttl == 0 {
		return 0, 0
	}
	cutoff := time.Now().Add(-ttl)
	var count int
	var bytes int64
	_ = filepath.WalkDir(b.cacheDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Context cancellation between files — bail without losing what
		// we already removed.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if rerr := os.Remove(p); rerr == nil {
				count++
				bytes += info.Size()
			}
		}
		return nil
	})
	return count, bytes
}

// evictByDiskPressure removes oldest-mtime files when the filesystem
// holding the cache is above limit%. Stops once usage falls below
// (limit-5)%. Returns counts plus the before/after usage ratios so the
// caller can log them.
func (b *S3Backend) evictByDiskPressure(ctx context.Context, limitPct int) (int, int64, float64, float64) {
	if limitPct <= 0 || limitPct >= 100 {
		return 0, 0, 0, 0
	}
	usageBefore, err := diskUsageFraction(b.cacheDir)
	if err != nil {
		slog.Warn("media.cache.statfs_failed", "error", err)
		return 0, 0, 0, 0
	}
	limit := float64(limitPct) / 100.0
	if usageBefore < limit {
		return 0, 0, usageBefore, usageBefore
	}

	// 5-point hysteresis: evict down to (limit - 0.05) so we don't flap
	// when traffic refills the cache right after the sweep.
	target := limit - 0.05
	if target < 0.05 {
		target = 0.05
	}

	files, err := collectCacheFiles(b.cacheDir)
	if err != nil {
		slog.Warn("media.cache.walk_failed", "error", err)
		return 0, 0, usageBefore, usageBefore
	}
	// Oldest first.
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })

	var count int
	var bytes int64
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		if rerr := os.Remove(f.path); rerr == nil {
			count++
			bytes += f.size
		}
		// Re-check the filesystem every 50 deletions — cheap enough but
		// not per-file (statfs isn't free).
		if count%50 == 0 {
			if u, uerr := diskUsageFraction(b.cacheDir); uerr == nil && u < target {
				usageAfter := u
				return count, bytes, usageBefore, usageAfter
			}
		}
	}
	usageAfter, _ := diskUsageFraction(b.cacheDir)
	return count, bytes, usageBefore, usageAfter
}

type cacheFile struct {
	path    string
	size    int64
	modTime time.Time
}

func collectCacheFiles(root string) ([]cacheFile, error) {
	var out []cacheFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out = append(out, cacheFile{path: p, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	return out, err
}

// removeEmptyDirs walks the cache and removes empty <sessionHash>/
// directories left behind after eviction. Best-effort: failures are
// logged-and-ignored. We never remove the cache root itself.
func (b *S3Backend) removeEmptyDirs() {
	entries, err := os.ReadDir(b.cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(b.cacheDir, e.Name())
		inner, ierr := os.ReadDir(sub)
		if ierr != nil || len(inner) > 0 {
			continue
		}
		if rerr := os.Remove(sub); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			slog.Debug("media.cache.rmdir_skipped", "dir", sub, "error", rerr)
		}
	}
}

// diskUsageFraction is defined per-OS in sweeper_statfs_{unix,windows}.go.

// roundFloat rounds f to the given decimal places. Used for log
// readability — saves operators counting digits in dashboards.
func roundFloat(f float64, decimals int) float64 {
	pow := 1.0
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	return float64(int(f*pow+0.5)) / pow
}
