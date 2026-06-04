ALTER TABLE skills
  ADD COLUMN IF NOT EXISTS source_url text,
  ADD COLUMN IF NOT EXISTS source_sha varchar(64),
  ADD COLUMN IF NOT EXISTS source_ref text,
  ADD COLUMN IF NOT EXISTS installed_by uuid,
  ADD COLUMN IF NOT EXISTS installed_at timestamptz;

CREATE TABLE IF NOT EXISTS skill_install_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     uuid,
    skill_slug  varchar(128) NOT NULL,
    event_type  varchar(32) NOT NULL,
    source_url  text,
    source_sha  varchar(64),
    metadata    jsonb DEFAULT '{}',
    created_at  timestamptz DEFAULT now()
);

CREATE INDEX IF NOT EXISTS skill_install_events_tenant_idx ON skill_install_events (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS skill_install_events_slug_idx ON skill_install_events (skill_slug);
