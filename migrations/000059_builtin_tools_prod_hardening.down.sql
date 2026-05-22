-- Roll back the prod-hardening pin: revert each entry to enabled = true
-- (the Go seedBuiltinTools default), letting the next startup re-seed
-- whatever defaults are current in code.
--
-- WARNING: this re-enables `exec`, all four skill tools, and the
-- browser tool. Only run this when the supporting runtime is in place
-- (full-skills image with python3 / sandbox / Playwright). Otherwise
-- the agent will surface broken tools again.

UPDATE builtin_tools
   SET enabled    = true,
       updated_at = NOW()
 WHERE name IN ('exec', 'skill_search', 'publish_skill', 'skill_manage',
                'use_skill', 'browser', 'read_image');
