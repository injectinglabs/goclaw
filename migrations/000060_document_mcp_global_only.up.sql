-- 000060_document_mcp_global_only.up.sql
--
-- Collapse all per-tenant document-mcp rows into a single global row
-- without losing agent/user grants. Document-mcp is a stateless sidecar
-- that gets caller identity from the per-call X-Actor-* headers goclaw
-- injects via mcp-go's WithHTTPHeaderFunc — no per-tenant connection
-- pool is needed. Six rows (one per tenant) with empty headers={} were
-- a leftover from a pre-actor-headers era and produce six separate
-- pools to the same upstream URL.
--
-- The migration is idempotent: re-running it after a successful pass is
-- a no-op. All steps run in a single transaction so an interrupted
-- migration leaves the database exactly where it started.
--
-- Steps:
--   1. Upsert the global row (tenant_id IS NULL).
--   2. Copy every mcp_agent_grants row pointing at a per-tenant
--      document-mcp server onto the global server. Same for
--      mcp_user_grants. Conflicts (same agent already granted via the
--      global row from a previous attempt) are ignored.
--   3. Delete the per-tenant rows. ON DELETE CASCADE wipes the old
--      grants — they're already on the global row, so no access loss.
--
-- Out of scope: mcp_access_requests pointing at per-tenant rows are
-- left to cascade-delete. They're pending grant requests, not
-- granted access; losing them just means the agent has to re-request
-- if they care. Acceptable.

BEGIN;

DO $$
DECLARE
    global_id UUID;
BEGIN
    -- 1. Make sure the global row exists. Upsert keeps url/transport
    --    fresh in case they ever drift on a per-tenant row.
    INSERT INTO mcp_servers (name, transport, url, headers, enabled, tenant_id, created_by, created_at, updated_at)
    VALUES ('document-mcp', 'streamable-http', 'http://document-mcp:9200/mcp', '{}'::jsonb, true, NULL, 'migration_000060', NOW(), NOW())
    ON CONFLICT (name) WHERE tenant_id IS NULL
    DO UPDATE SET
        url       = EXCLUDED.url,
        transport = EXCLUDED.transport,
        enabled   = true,
        updated_at = NOW();

    SELECT id INTO global_id
    FROM mcp_servers
    WHERE name = 'document-mcp' AND tenant_id IS NULL;

    IF global_id IS NULL THEN
        RAISE EXCEPTION 'failed to upsert global document-mcp row';
    END IF;

    -- 2a. Copy agent grants from every per-tenant row to the global row.
    INSERT INTO mcp_agent_grants (
        server_id, agent_id, enabled, tool_allow, tool_deny,
        config_overrides, granted_by
    )
    SELECT
        global_id, g.agent_id, g.enabled, g.tool_allow, g.tool_deny,
        g.config_overrides,
        COALESCE(g.granted_by, 'migration_000060')
    FROM mcp_agent_grants g
    INNER JOIN mcp_servers s ON s.id = g.server_id
    WHERE s.name = 'document-mcp' AND s.tenant_id IS NOT NULL
    ON CONFLICT (server_id, agent_id) DO NOTHING;

    -- 2b. Same for user grants.
    INSERT INTO mcp_user_grants (
        server_id, user_id, enabled, tool_allow, tool_deny, granted_by
    )
    SELECT
        global_id, g.user_id, g.enabled, g.tool_allow, g.tool_deny,
        COALESCE(g.granted_by, 'migration_000060')
    FROM mcp_user_grants g
    INNER JOIN mcp_servers s ON s.id = g.server_id
    WHERE s.name = 'document-mcp' AND s.tenant_id IS NOT NULL
    ON CONFLICT (server_id, user_id) DO NOTHING;

    -- 3. Drop the per-tenant rows. ON DELETE CASCADE removes their
    --    grants/access_requests; equivalent grants are on global_id now.
    DELETE FROM mcp_servers
    WHERE name = 'document-mcp' AND tenant_id IS NOT NULL;
END $$;

COMMIT;
