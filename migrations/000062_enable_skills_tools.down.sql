-- Revert: re-disable skill discovery/activation (back to the 000059 hardened state).
UPDATE builtin_tools
  SET enabled = false, updated_at = NOW()
  WHERE name IN ('skill_search', 'use_skill');
