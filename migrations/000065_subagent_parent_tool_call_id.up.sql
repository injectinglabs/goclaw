-- Persist the parent agent's spawn tool_call.id so the website's
-- sessions.preview can correlate a spawn ToolCall in the assistant
-- message back to the structured subagent_tasks row, and rebuild the
-- nested mini-chat UI after page reload without parsing markdown text.
--
-- Lives in `metadata` JSONB today (in-memory only at runtime), but
-- promoting it to a column gives us an index + cleaner JOIN. Tool
-- history (per-step name/status/duration) goes in a separate JSONB
-- column so the UI's tool timeline can be rebuilt structurally.
ALTER TABLE subagent_tasks
    ADD COLUMN IF NOT EXISTS parent_tool_call_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS tool_history        JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS thinking            TEXT;

-- Cover the website preview lookup: given a session's spawn tool_call.id,
-- find the matching subagent_task. Partial index excludes the very common
-- NULL state (sync subagents / RunSync path) to keep the index tight.
CREATE INDEX IF NOT EXISTS idx_subagent_tasks_parent_tool_call_id
    ON subagent_tasks (parent_tool_call_id)
    WHERE parent_tool_call_id IS NOT NULL;
