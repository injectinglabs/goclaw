-- Pin the prod-hardened set of builtin_tools.
--
-- Why this migration exists
-- -------------------------
-- `cmd/gateway_builtin_tools.go:seedBuiltinTools` runs on every startup and
-- inserts default rows for every builtin tool the Go code knows about,
-- using `enabled=true` for almost everything. The seed uses
-- INSERT ON CONFLICT DO NOTHING, so the FIRST startup of a fresh
-- environment lands every tool in the "enabled" state — including a
-- handful that are unsafe or non-functional in our prod build (no
-- python3 / no Playwright / no exec sandbox).
--
-- For existing environments this migration aligns the current state with
-- the desired state. Operators have historically had to apply these
-- UPDATEs by hand after each fresh deploy; capturing them as a tracked
-- migration means a new stage / prod / disaster-recovery spin-up reaches
-- the same row state automatically.
--
-- Idempotent: INSERT … ON CONFLICT (name) DO UPDATE SET enabled =
-- EXCLUDED.enabled. If the row is missing we INSERT with the desired
-- enabled value; if it exists (Go seed already ran) we OVERWRITE just
-- the `enabled` column. Display name / description / category fall back
-- to placeholder values that the Go seeder will UPDATE on the next
-- startup pass via its own ON CONFLICT clauses, so we don't have to
-- mirror the Go-side strings here.
--
-- This migration runs ONCE (tracked in schema_migrations). After it
-- lands, operators can still flip rows via `UPDATE builtin_tools SET
-- enabled = ... WHERE name = ...` and those edits will survive subsequent
-- restarts (the Go seeder uses ON CONFLICT DO NOTHING and never
-- overwrites enabled on an existing row).
--
-- The set below is the union of stage and prod hardening as of
-- 2026-05-22 — both environments agree on it.

INSERT INTO builtin_tools (name, display_name, description, category, enabled)
VALUES
  -- Arbitrary shell execution. Re-enable only inside a real sandbox
  -- (skills runtime + exec_approval gate).
  ('exec',          'Execute Command', 'Execute a shell command in the workspace and return stdout/stderr', 'runtime', false),

  -- Skill management surface. Useful only when the skills runtime
  -- (python3 + bundled deps) is actually present. The minimal prod
  -- image ships without it, so leave these off — surfacing them would
  -- let the agent attempt skill calls that always fail.
  ('skill_search',  'Skill Search',    'Search for available skills by keyword or description',              'skills',  false),
  ('publish_skill', 'Publish Skill',   'Register a skill directory in the system database',                  'skills',  false),
  ('skill_manage',  'Skill Manager',   'Create, patch, or delete skills from conversation experience',       'skills',  false),
  ('use_skill',     'Use Skill',       'Invoke a registered skill by name',                                  'skills',  false),

  -- Headless browser tool. Needs Playwright/Chromium infra we don't
  -- run in this image. Off by default; flip on once a browser
  -- sidecar (or full-skills image with playwright) is wired.
  ('browser',       'Browser',         'Browser automation tool',                                            'web',     false),

  -- Vision / image-OCR via llm-service-web's `vision` alias (Llama 4
  -- Scout via OpenRouter). No local runtime needed — pure HTTP call.
  -- Safe to ship enabled by default.
  ('read_image',    'Read Image',      'Vision / OCR on an attached image via llm-service-web vision alias', 'media',   true)

ON CONFLICT (name) DO UPDATE
  SET enabled    = EXCLUDED.enabled,
      updated_at = NOW();
