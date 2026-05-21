BEGIN;

-- Refuse rollback if any global rows exist; they cannot be re-assigned a
-- tenant_id automatically, and dropping them silently would lose data.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM mcp_servers WHERE tenant_id IS NULL LIMIT 1) THEN
        RAISE EXCEPTION
            'Cannot roll back migration 000058: mcp_servers contains global rows '
            '(tenant_id IS NULL). Remove or reassign them first.';
    END IF;
END;
$$;

-- Restore the original composite unique index.
DROP INDEX IF EXISTS idx_mcp_servers_tenant_name;
DROP INDEX IF EXISTS idx_mcp_servers_global_name;
DROP INDEX IF EXISTS idx_mcp_servers_global;

CREATE UNIQUE INDEX idx_mcp_servers_tenant_name
    ON mcp_servers (tenant_id, name);

-- Restore NOT NULL constraint.
ALTER TABLE mcp_servers ALTER COLUMN tenant_id SET NOT NULL;

COMMIT;
