package cmd

import (
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/media"
)

// mediaSweeperConfigFromEnv reads the three sweeper knobs from env and
// applies sane defaults.
//
//	GOCLAW_MEDIA_CACHE_SWEEP_INTERVAL  (duration, default "0" = disabled)
//	GOCLAW_MEDIA_CACHE_TTL             (duration, default "720h" = 30 days)
//	GOCLAW_MEDIA_CACHE_DISK_LIMIT      (percent 1..99, default 70)
//
// The interval is the master switch: with 0 the sweeper goroutine never
// starts, regardless of the other two settings. This keeps the new
// behaviour opt-in until operators flip it on.
func mediaSweeperConfigFromEnv() media.SweeperConfig {
	cfg := media.SweeperConfig{
		Interval:         parseDurationEnv("GOCLAW_MEDIA_CACHE_SWEEP_INTERVAL", 0),
		TTL:              parseDurationEnv("GOCLAW_MEDIA_CACHE_TTL", 720*time.Hour),
		DiskLimitPercent: parseIntEnv("GOCLAW_MEDIA_CACHE_DISK_LIMIT", 70),
	}
	// Clamp the disk limit to a sane range. Anything <=0 or >=100 is
	// almost certainly an operator typo, and we silently treat it as
	// "disable disk-pressure phase" rather than panicking.
	if cfg.DiskLimitPercent <= 0 || cfg.DiskLimitPercent >= 100 {
		slog.Warn("media.cache.disk_limit_out_of_range",
			"value", cfg.DiskLimitPercent, "action", "disabling disk-pressure phase")
		cfg.DiskLimitPercent = 0
	}
	return cfg
}

func parseDurationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("env.parse_duration_failed", "name", name, "value", v, "error", err, "fallback", def)
		return def
	}
	return d
}

func parseIntEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("env.parse_int_failed", "name", name, "value", v, "error", err, "fallback", def)
		return def
	}
	return n
}
