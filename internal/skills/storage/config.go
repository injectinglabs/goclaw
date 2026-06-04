// Package storage durably mirrors per-tenant skill directories to S3 so
// every node in the ASG can serve a skill the user installed on a
// different node. The local filesystem stays the read path (the agent's
// read_file tool + the BM25 skill index both want plain paths), so this
// package only intercepts writes (install / update / delete) and the
// startup warm-up that backfills missing local copies from S3.
//
// S3 layout — one object per file under the skill tree:
//
//	{prefix}/tenants/{tenant_slug}/skills-store/{slug}/{version}/SKILL.md
//	{prefix}/tenants/{tenant_slug}/skills-store/{slug}/{version}/scripts/foo.py
//	...
//
// Versions are pinned in DB so the prefix is content-addressable: an
// older version stays installable until its DB row is deleted, which
// gives us rollback for free.
package storage

import (
	"os"
	"strings"
)

// Config selects and parameterises the S3 mirror. An empty Bucket means
// "feature disabled" — install/update still work, but only against the
// local filesystem (lite / desktop / single-node SaaS deployments).
type Config struct {
	Bucket   string
	Prefix   string // default: "skills"
	Region   string // default: us-east-1
	Endpoint string // S3-compatible services (MinIO etc.) — usually empty
}

// LoadConfigFromEnv reads the dedicated GOCLAW_SKILLS_S3_* variables.
// We keep them separate from GOCLAW_MEDIA_S3_* so operators can disable
// the skills mirror without touching the media one, or vice-versa, and
// so the bucket choice can diverge if we ever want a colder tier for
// skill archives.
func LoadConfigFromEnv() Config {
	cfg := Config{
		Bucket:   strings.TrimSpace(os.Getenv("GOCLAW_SKILLS_S3_BUCKET")),
		Prefix:   strings.TrimSpace(os.Getenv("GOCLAW_SKILLS_S3_PREFIX")),
		Region:   strings.TrimSpace(os.Getenv("GOCLAW_SKILLS_S3_REGION")),
		Endpoint: strings.TrimSpace(os.Getenv("GOCLAW_SKILLS_S3_ENDPOINT")),
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "skills"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return cfg
}

// Enabled reports whether the mirror is configured. Callers gate writes
// and the startup prefetch on this: when false, skills are local-only.
func (c Config) Enabled() bool { return c.Bucket != "" }
