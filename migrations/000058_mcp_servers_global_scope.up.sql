BEGIN;

-- Allow tenant_id to be NULL.
-- NULL = global row: visible to all tenants, mutable only by platform admin.
ALTER TABLE mcp_servers ALTER COLUMN tenant_id DROP NOT NULL;

-- Replace the single composite unique index with two partial indexes so that:
--   • per-tenant names stay unique within their tenant
--   • global names stay unique across the whole table
--   Both constraints are enforced independently by the DB engine.
DROP INDEX IF EXISTS idx_mcp_servers_tenant_name;

CREATE UNIQUE INDEX idx_mcp_servers_tenant_name
    ON mcp_servers (tenant_id, name)
    WHERE tenant_id IS NOT NULL;

CREATE UNIQUE INDEX idx_mcp_servers_global_name
    ON mcp_servers (name)
    WHERE tenant_id IS NULL;

-- Fast lookup of all global rows (used by scope queries with OR IS NULL).
CREATE INDEX IF NOT EXISTS idx_mcp_servers_global
    ON mcp_servers (name)
    WHERE tenant_id IS NULL;

COMMIT;
