-- Persist the exact prompt-token count returned by the upstream provider
-- after every chat run. Replaces the byte-based fallback in the
-- sessions.list estimated_tokens column so the UI context indicator
-- stops drifting between page reloads (LENGTH(messages)/4 + 12000 was
-- rough; usage.prompt_tokens is the same number the model bills against).
--
-- NULL = no run has completed on this session yet → SELECT falls back
-- to the legacy octet_length-based estimate so newly-created sessions
-- still get a non-zero indicator.
ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS last_prompt_tokens BIGINT,
    ADD COLUMN IF NOT EXISTS last_message_count INT;
