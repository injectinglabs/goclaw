-- Re-enable the skill discovery/activation builtin tools.
--
-- Migration 000059 (builtin_tools prod hardening) disabled the entire
-- skills category with the rationale: "Useful only when the skills runtime
-- (python3 + bundled deps) is actually present. The minimal prod image
-- ships without it." That premise no longer holds — the goclaw image is now
-- built with ENABLE_PYTHON + ENABLE_SANDBOX, and a per-session sandbox is
-- wired in (see injecting-ai-goclaw deploy.yml / launch-template), so skills
-- can run. With the category disabled, skill_search/use_skill were filtered
-- out of every agent's toolset (applyBuiltinToolDisables), so no agent could
-- ever discover or run a skill regardless of how skills were provisioned.
--
-- Enable only discovery + activation here:
--   skill_search — find a skill by keyword
--   use_skill    — activate a found skill (agent then read_file's its SKILL.md)
-- Authoring tools (skill_manage, publish_skill) intentionally stay disabled —
-- skills are created via the dashboard UI / HTTP upload, not by the agent.
-- exec also stays disabled here; code-execution skills need it, but exec must
-- be re-enabled together with the sandbox, which is not on prod yet.
--
-- Global table (no tenant_id) → applies to every tenant. Idempotent.

INSERT INTO builtin_tools (name, display_name, description, category, enabled)
VALUES
  ('skill_search', 'Skill Search', 'Search for available skills by keyword or description', 'skills', true),
  ('use_skill',    'Use Skill',    'Invoke a registered skill by name',                    'skills', true)
ON CONFLICT (name) DO UPDATE
  SET enabled    = EXCLUDED.enabled,
      updated_at = NOW();
