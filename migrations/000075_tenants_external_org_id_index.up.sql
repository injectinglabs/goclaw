-- Expression index on tenants.settings->>'external_org_id' so the
-- reverse lookup used by /v1/internal/workflows/enqueue (and any
-- future sidecar-back-to-goclaw flow) stays O(log n) instead of a
-- full table scan + JSONB extract per row.
--
-- external_org_id is the web-backend's organizations.id UUID stamped
-- by auth-proxy on every login. It is THE canonical tenant identity
-- across the multi-service surface (LLM service, web-agent-api,
-- sheets-mcp). Goclaw's local tenants.id is an implementation detail;
-- inbound API boundaries should accept external_org_id and reverse-
-- resolve to the local UUID.
--
-- Not UNIQUE: during the org-mode rollout some tenants may transiently
-- share the same external_org_id while auth-proxy re-stamps. The
-- lookup picks the first match — that's acceptable because all rows
-- in such a window will converge to the same tenant within seconds.
CREATE INDEX IF NOT EXISTS idx_tenants_external_org_id
  ON tenants ((settings->>'external_org_id'))
  WHERE settings->>'external_org_id' IS NOT NULL;
