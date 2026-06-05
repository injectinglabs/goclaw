package storage

import (
	"testing"
)

func TestConfig_EnabledFlag(t *testing.T) {
	t.Parallel()
	if (Config{}).Enabled() {
		t.Fatal("empty Config must report Enabled=false")
	}
	if !(Config{Bucket: "b"}).Enabled() {
		t.Fatal("Config with Bucket must report Enabled=true")
	}
}

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	// Cannot t.Parallel() — Setenv mutates process env.
	t.Setenv("GOCLAW_SKILLS_S3_BUCKET", "")
	t.Setenv("GOCLAW_SKILLS_S3_PREFIX", "")
	t.Setenv("GOCLAW_SKILLS_S3_REGION", "")
	cfg := LoadConfigFromEnv()
	if cfg.Prefix != "skills" {
		t.Fatalf("default prefix = %q, want %q", cfg.Prefix, "skills")
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("default region = %q", cfg.Region)
	}
	if cfg.Enabled() {
		t.Fatal("config without bucket must be Enabled=false")
	}
}

func TestLoadConfigFromEnv_Overrides(t *testing.T) {
	// Cannot t.Parallel() — Setenv mutates process env. Region is left
	// unset deliberately: the whole stack deploys to us-east-1 (matches
	// the existing media bucket and the EC2 ASG), so a "non-default"
	// region in tests would just confuse a reader. We verify only the
	// dimensions that actually vary per deployment: bucket + prefix.
	t.Setenv("GOCLAW_SKILLS_S3_BUCKET", "my-bucket")
	t.Setenv("GOCLAW_SKILLS_S3_PREFIX", "custom/skills")
	cfg := LoadConfigFromEnv()
	if cfg.Bucket != "my-bucket" || cfg.Prefix != "custom/skills" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("region default broken: %q", cfg.Region)
	}
	if !cfg.Enabled() {
		t.Fatal("Enabled must be true when bucket is set")
	}
}

// TestMirror_SkillKeyPrefix locks down the on-disk-key contract.
// Anything that resolves a download from S3 (startup warm-up, future
// admin tooling) depends on this exact shape, so a silent rename of the
// layout would orphan every existing object.
func TestMirror_SkillKeyPrefix(t *testing.T) {
	t.Parallel()
	m := &Mirror{prefix: "skills"}
	got := m.SkillKeyPrefix("test-org-ilya", "algorithmic-art", 1)
	want := "skills/tenants/test-org-ilya/skills-store/algorithmic-art/1"
	if got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
}

func TestMirror_SkillKeyPrefix_EmptyTopPrefix(t *testing.T) {
	t.Parallel()
	// Some operators may want the bucket root used as the prefix; the
	// formatter keeps the leading slash off so callers can't accidentally
	// emit "//tenants/...".
	m := &Mirror{prefix: ""}
	got := m.SkillKeyPrefix("t", "s", 2)
	want := "/tenants/t/skills-store/s/2"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
