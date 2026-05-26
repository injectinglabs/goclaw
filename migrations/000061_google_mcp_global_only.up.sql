-- 000061_google_mcp_global_only.up.sql
--
-- Collapse all per-tenant Google/Slack MCP rows into single global rows
-- (tenant_id IS NULL), the same cleanup migration 000060 did for
-- document-mcp. These six sidecars (gmail/calendar/sheets/docs/drive/
-- slack) get the calling identity from the per-call X-Actor-* headers
-- goclaw injects via mcp-go's WithHTTPHeaderFunc and from the per-user
-- mcp_user_credentials (OAuth proxy headers) — they do NOT need a
-- per-tenant server row. The per-tenant rows were created by an older
-- provisioning path that CREATE'd rows; the current path only GRANTS
-- pre-existing rows, so new tenants (which only ever had global rows
-- to grant) never received Google MCP access. Collapsing to global
-- rows lets provisionStandardMCPServers grant them like document-mcp.
--
-- Unlike 000060, these rows carry encrypted `headers` (the shared
-- service token, AES-GCM with a per-row nonce). The token plaintext is
-- identical across tenants, so the global row copies the encrypted
-- headers (+ url/transport/env/settings/...) verbatim from one existing
-- per-tenant row — it decrypts to the same valid token under the shared
-- GOCLAW_ENCRYPTION_KEY.
--
-- Idempotent: re-running after a successful pass is a no-op (the source
-- SELECT finds no per-tenant rows, so the loop CONTINUEs). All steps run
-- in one transaction.
--
-- Steps, per server name:
--   1. Copy one per-tenant row's config into a global row (upsert).
--   2. Copy mcp_agent_grants / mcp_user_grants / mcp_user_credentials
--      from the per-tenant rows onto the global row, preserving the
--      grant/credential tenant_id (NOT NULL there). Conflicts ignored.
--   3. Delete the per-tenant rows. ON DELETE CASCADE wipes their old
--      grants/credentials — equivalents are on the global row now.
--
-- Out of scope: mcp_access_requests pointing at per-tenant rows cascade-
-- delete (pending requests, not granted access — acceptable to drop).

BEGIN;

DO $$
DECLARE
    svc_name  TEXT;
    global_id UUID;
    src       mcp_servers%ROWTYPE;
    names     TEXT[] := ARRAY[
        'gmail-mcp', 'calendar-mcp', 'sheets-mcp',
        'docs-mcp', 'drive-mcp', 'slack-mcp'
    ];
BEGIN
    FOREACH svc_name IN ARRAY names LOOP
        -- Pick one per-tenant row to copy config from (encrypted headers,
        -- url, transport, env, settings, ...). Deterministic: oldest row.
        SELECT * INTO src
        FROM mcp_servers
        WHERE name = svc_name AND tenant_id IS NOT NULL
        ORDER BY created_at ASC, id ASC
        LIMIT 1;

        IF src.id IS NULL THEN
            -- Nothing per-tenant to collapse for this name (already done,
            -- or never existed). Leave any existing global row untouched.
            CONTINUE;
        END IF;

        -- 1. Upsert the global row from the source per-tenant row.
        INSERT INTO mcp_servers (
            name, display_name, transport, command, args, url, headers, env,
            api_key, tool_prefix, timeout_sec, settings, enabled, tenant_id,
            created_by, created_at, updated_at
        )
        VALUES (
            src.name, src.display_name, src.transport, src.command, src.args,
            src.url, src.headers, src.env, src.api_key, src.tool_prefix,
            src.timeout_sec, src.settings, true, NULL,
            'migration_000061', NOW(), NOW()
        )
        ON CONFLICT (name) WHERE tenant_id IS NULL
        DO UPDATE SET
            display_name = EXCLUDED.display_name,
            transport    = EXCLUDED.transport,
            command      = EXCLUDED.command,
            args         = EXCLUDED.args,
            url          = EXCLUDED.url,
            headers      = EXCLUDED.headers,
            env          = EXCLUDED.env,
            api_key      = EXCLUDED.api_key,
            tool_prefix  = EXCLUDED.tool_prefix,
            timeout_sec  = EXCLUDED.timeout_sec,
            settings     = EXCLUDED.settings,
            enabled      = true,
            updated_at   = NOW();

        SELECT id INTO global_id
        FROM mcp_servers
        WHERE name = svc_name AND tenant_id IS NULL;

        IF global_id IS NULL THEN
            RAISE EXCEPTION 'failed to upsert global % row', svc_name;
        END IF;

        -- 2a. Agent grants. tenant_id is NOT NULL on the grant — preserve
        --     it from the source grant (the global server's tenant_id is
        --     NULL, but the grant keeps pointing at the agent's tenant).
        INSERT INTO mcp_agent_grants (
            server_id, agent_id, tenant_id, enabled, tool_allow, tool_deny,
            config_overrides, granted_by
        )
        SELECT
            global_id, g.agent_id, g.tenant_id, g.enabled, g.tool_allow,
            g.tool_deny, g.config_overrides,
            COALESCE(g.granted_by, 'migration_000061')
        FROM mcp_agent_grants g
        INNER JOIN mcp_servers s ON s.id = g.server_id
        WHERE s.name = svc_name AND s.tenant_id IS NOT NULL
        ON CONFLICT (server_id, agent_id) DO NOTHING;

        -- 2b. User grants — tenant_id also NOT NULL.
        INSERT INTO mcp_user_grants (
            server_id, user_id, tenant_id, enabled, tool_allow, tool_deny,
            granted_by
        )
        SELECT
            global_id, g.user_id, g.tenant_id, g.enabled, g.tool_allow,
            g.tool_deny, COALESCE(g.granted_by, 'migration_000061')
        FROM mcp_user_grants g
        INNER JOIN mcp_servers s ON s.id = g.server_id
        WHERE s.name = svc_name AND s.tenant_id IS NOT NULL
        ON CONFLICT (server_id, user_id) DO NOTHING;

        -- 2c. Per-user credentials (OAuth proxy headers). Keyed by
        --     (server_id, user_id, tenant_id) — preserve user_id/tenant_id.
        INSERT INTO mcp_user_credentials (
            server_id, user_id, tenant_id, api_key, headers, env,
            created_at, updated_at
        )
        SELECT
            global_id, c.user_id, c.tenant_id, c.api_key, c.headers, c.env,
            NOW(), NOW()
        FROM mcp_user_credentials c
        INNER JOIN mcp_servers s ON s.id = c.server_id
        WHERE s.name = svc_name AND s.tenant_id IS NOT NULL
        ON CONFLICT (server_id, user_id, tenant_id) DO NOTHING;

        -- 3. Drop the per-tenant rows. CASCADE removes their old grants/
        --    credentials/access_requests; equivalents live on global_id.
        DELETE FROM mcp_servers
        WHERE name = svc_name AND tenant_id IS NOT NULL;
    END LOOP;
END $$;

COMMIT;
