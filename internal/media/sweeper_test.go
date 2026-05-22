package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestBackend(t *testing.T) *S3Backend {
	t.Helper()
	dir := t.TempDir()
	// Construct a minimal S3Backend without touching AWS — only cacheDir
	// matters for the sweeper.
	return &S3Backend{cacheDir: dir}
}

// touchCacheFile writes a zero-padded blob into <cacheDir>/<hash>/<id>.<ext>
// with a specific mtime so tests can drive the TTL phase.
func touchCacheFile(t *testing.T, b *S3Backend, hash, name string, size int, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(b.cacheDir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mtime := time.Now().Add(-age)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return p
}

func TestEvictByTTL_RemovesOldFilesKeepsFresh(t *testing.T) {
	b := newTestBackend(t)

	old := touchCacheFile(t, b, "abc123", "01.jpg", 64, 48*time.Hour)
	fresh := touchCacheFile(t, b, "abc123", "02.jpg", 64, 30*time.Minute)

	count, bytes := b.evictByTTL(context.Background(), 24*time.Hour)
	if count != 1 || bytes != 64 {
		t.Fatalf("want 1/64 deleted, got %d/%d", count, bytes)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expected old file removed, err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("expected fresh file kept, err=%v", err)
	}
}

func TestEvictByTTL_ZeroDisablesPhase(t *testing.T) {
	b := newTestBackend(t)
	p := touchCacheFile(t, b, "h", "f.jpg", 1, 365*24*time.Hour)
	count, _ := b.evictByTTL(context.Background(), 0)
	if count != 0 {
		t.Fatalf("ttl=0 should be no-op, got %d deletions", count)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file should be intact: %v", err)
	}
}

func TestRemoveEmptyDirs_DropsEmptiedSessionHashes(t *testing.T) {
	b := newTestBackend(t)
	p := touchCacheFile(t, b, "empty-after", "f.jpg", 8, 10*time.Hour)
	touchCacheFile(t, b, "stays", "g.jpg", 8, 10*time.Minute)

	if err := os.Remove(p); err != nil {
		t.Fatalf("setup: %v", err)
	}
	b.removeEmptyDirs()

	if _, err := os.Stat(filepath.Join(b.cacheDir, "empty-after")); !os.IsNotExist(err) {
		t.Fatalf("empty session-hash dir should be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(b.cacheDir, "stays")); err != nil {
		t.Fatalf("non-empty dir should be kept, err=%v", err)
	}
}

func TestCollectCacheFiles_OldestFirstSortStable(t *testing.T) {
	b := newTestBackend(t)
	a := touchCacheFile(t, b, "h", "a", 1, 5*time.Hour)
	c := touchCacheFile(t, b, "h", "c", 1, 1*time.Hour)
	bp := touchCacheFile(t, b, "h", "b", 1, 3*time.Hour)

	files, err := collectCacheFiles(b.cacheDir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 files, got %d", len(files))
	}
	// Caller sorts; verify the per-file ModTime captured the right one.
	byPath := map[string]time.Time{}
	for _, f := range files {
		byPath[f.path] = f.modTime
	}
	if !byPath[a].Before(byPath[bp]) || !byPath[bp].Before(byPath[c]) {
		t.Fatalf("modTime ordering mismatch: a=%v b=%v c=%v",
			byPath[a], byPath[bp], byPath[c])
	}
}

func TestEvictByDiskPressure_NoOpBelowLimit(t *testing.T) {
	b := newTestBackend(t)
	p := touchCacheFile(t, b, "h", "f.jpg", 8, 1*time.Hour)
	// 1% limit — almost certainly above; if not, we still expect non-error
	// behavior (just may delete the file). Use a clearly-not-triggered
	// value: 99% — even a full host shouldn't be at 99% in CI.
	count, _, _, _ := b.evictByDiskPressure(context.Background(), 99)
	if count != 0 {
		t.Fatalf("expected no eviction below 99%% disk, got %d", count)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file should remain, err=%v", err)
	}
}

func TestSweepOnce_RunsAllPhasesWithoutPanic(t *testing.T) {
	b := newTestBackend(t)
	touchCacheFile(t, b, "h1", "old.jpg", 8, 48*time.Hour)
	touchCacheFile(t, b, "h2", "new.jpg", 8, 5*time.Minute)

	// Should not panic, should leave the fresh file, should remove old.
	b.SweepOnce(context.Background(), SweeperConfig{
		Interval:         time.Hour,
		TTL:              24 * time.Hour,
		DiskLimitPercent: 99, // unreachable in CI
	})

	if _, err := os.Stat(filepath.Join(b.cacheDir, "h1", "old.jpg")); !os.IsNotExist(err) {
		t.Fatalf("old file should be deleted, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(b.cacheDir, "h2", "new.jpg")); err != nil {
		t.Fatalf("fresh file should remain, err=%v", err)
	}
}
