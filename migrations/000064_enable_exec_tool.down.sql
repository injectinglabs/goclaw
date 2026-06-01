-- Revert: re-disable exec (back to the 000059 hardened state).
UPDATE builtin_tools
  SET enabled = false, updated_at = NOW()
  WHERE name = 'exec';
