-- 000074_sheets_mcp_default_grants.up.sql
--
-- Backfill mcp_agent_grants for the global sheets-mcp row onto every
-- tenant's default agent that already has a document-mcp grant.
--
-- Why: document-mcp + composio-mcp get auto-granted to new tenants by
-- the upstream provisioning path (auth-proxy / web-backend tenant-
-- create handler), but sheets-mcp was never added to that list. As a
-- result, the agent never sees sheets_create_spreadsheet,
-- sheets_enrich_run, etc. — and falls back to the Composio Google
-- Sheets tools, which are bottom-up cell-by-cell calls (no Sheet
-- Workflows orchestrator path). The 100 stale sheets-mcp grants in
-- prod predate that gap; the most recent 3 tenants have zero.
--
-- Fix: walk every (tenant_id, default-agent-with-document-mcp-grant)
-- and INSERT an enabled sheets-mcp grant if one doesn't exist already.
-- This is idempotent via ON CONFLICT (server_id, agent_id) DO NOTHING.
--
-- Scope: backfill only — fixing the upstream provisioning path so new
-- tenants get sheets-mcp on first-create is a separate change in the
-- web-backend tenant bootstrap. Once that's merged this migration
-- still serves as the single-shot catch-up for already-provisioned
-- tenants.
--
-- Bug class match: same pattern as the (parked) per-tenant grant
-- audit we ran in `scripts/verify_mcp` — operators can re-run that
-- script after applying to confirm sheets-mcp parity with document-mcp.

BEGIN;

DO $$
DECLARE
    sheets_id   UUID;
    document_id UUID;
    granted_n   INT;
BEGIN
    SELECT id INTO sheets_id
    FROM mcp_servers
    WHERE name = 'sheets-mcp' AND tenant_id IS NULL;

    SELECT id INTO document_id
    FROM mcp_servers
    WHERE name = 'document-mcp' AND tenant_id IS NULL;

    IF sheets_id IS NULL THEN
        -- No global sheets-mcp row → nothing to grant. Don't fail:
        -- a fresh dev DB may not have the row yet (000061 only
        -- materialises one if a per-tenant row existed to copy from).
        RAISE NOTICE 'sheets-mcp global row missing — skipping backfill';
        RETURN;
    END IF;

    IF document_id IS NULL THEN
        RAISE NOTICE 'document-mcp global row missing — skipping backfill';
        RETURN;
    END IF;

    -- Mirror document-mcp grants for the global sheets-mcp row.
    INSERT INTO mcp_agent_grants (
        server_id, agent_id, tenant_id, enabled, tool_allow, tool_deny,
        config_overrides, granted_by
    )
    SELECT
        sheets_id,
        g.agent_id,
        g.tenant_id,
        TRUE,
        g.tool_allow,
        g.tool_deny,
        g.config_overrides,
        'migration_000074'
    FROM mcp_agent_grants g
    WHERE g.server_id = document_id
      AND g.enabled = TRUE
    ON CONFLICT (server_id, agent_id) DO NOTHING;

    GET DIAGNOSTICS granted_n = ROW_COUNT;
    RAISE NOTICE 'sheets-mcp backfill: % new agent grants', granted_n;
END $$;

COMMIT;
