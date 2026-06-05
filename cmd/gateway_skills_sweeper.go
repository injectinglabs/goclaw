package cmd

import (
	"log/slog"

	skillstorage "github.com/nextlevelbuilder/goclaw/internal/skills/storage"
)

// skillsSweeperConfigFromEnv reads the skill-cache cleanup knobs from
// env. Mirrors the media sweeper convention so the operator only learns
// one mental model.
//
//	GOCLAW_SKILLS_CACHE_SWEEP_INTERVAL  (duration, default "0" = disabled)
//	GOCLAW_SKILLS_CACHE_DISK_LIMIT      (percent 1..99, default 70)
//
// Interval=0 is the master switch — the goroutine never starts. We keep
// the feature opt-in so a misconfigured threshold can't suddenly start
// deleting from local disk on an existing deployment.
func skillsSweeperConfigFromEnv() skillstorage.SweeperConfig {
	cfg := skillstorage.SweeperConfig{
		Interval:         parseDurationEnv("GOCLAW_SKILLS_CACHE_SWEEP_INTERVAL", 0),
		DiskLimitPercent: parseIntEnv("GOCLAW_SKILLS_CACHE_DISK_LIMIT", 70),
	}
	if cfg.DiskLimitPercent <= 0 || cfg.DiskLimitPercent >= 100 {
		slog.Warn("skills.cache.disk_limit_out_of_range",
			"value", cfg.DiskLimitPercent, "action", "disabling disk-pressure phase")
		cfg.DiskLimitPercent = 0
	}
	return cfg
}

