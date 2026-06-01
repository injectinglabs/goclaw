DROP INDEX IF EXISTS idx_subagent_tasks_parent_tool_call_id;

ALTER TABLE subagent_tasks
    DROP COLUMN IF EXISTS parent_tool_call_id,
    DROP COLUMN IF EXISTS tool_history,
    DROP COLUMN IF EXISTS thinking;
