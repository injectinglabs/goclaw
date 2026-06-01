-- Re-enable the exec tool now that a per-session sandbox is wired.
--
-- Migration 000059 (prod hardening) disabled exec with the note: "Re-enable
-- only inside a real sandbox (skills runtime + exec_approval gate)." That
-- sandbox is now deployed — the goclaw image is built with ENABLE_SANDBOX and
-- runs with GOCLAW_SANDBOX_MODE=all + the goclaw-sandbox image over the mounted
-- Docker socket (see injecting-ai-goclaw deploy.yml / launch-template). So exec
-- runs inside an ephemeral per-session container (network=false, cpu/mem caps),
-- not in the goclaw process. This unblocks code-execution skills (skills whose
-- SKILL.md runs bundled Python/scripts via exec).
--
-- ⚠️ SEQUENCING: builtin_tools is GLOBAL — this enables exec on any environment
-- that runs this migration. exec MUST run sandboxed, so the sandbox
-- (ENABLE_SANDBOX image + GOCLAW_SANDBOX_* env) must be deployed on an
-- environment BEFORE this reaches it. Stage has the sandbox today; prod must
-- receive the sandbox in the same promotion that brings this migration.

INSERT INTO builtin_tools (name, display_name, description, category, enabled)
VALUES ('exec', 'Execute Command', 'Execute a shell command in the workspace and return stdout/stderr', 'runtime', true)
ON CONFLICT (name) DO UPDATE
  SET enabled = true, updated_at = NOW();
