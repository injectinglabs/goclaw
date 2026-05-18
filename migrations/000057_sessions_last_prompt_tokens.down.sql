ALTER TABLE sessions
    DROP COLUMN IF EXISTS last_message_count,
    DROP COLUMN IF EXISTS last_prompt_tokens;
