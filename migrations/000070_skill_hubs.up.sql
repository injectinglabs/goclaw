-- skill_hubs: curated registry of external skill catalogs the install flow
-- can browse. Each row is one source URL (typically a marketplace.json /
-- index.json on GitHub raw). Admin-managed: there is no user-facing CRUD
-- endpoint — populated by us via SQL or future admin tools. Replaces the
-- hardcoded defaultMarketplaces slice in internal/http/skills_marketplace.go.

CREATE TABLE skill_hubs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    url         text NOT NULL UNIQUE,
    name        text NOT NULL,
    description text,
    -- trust_level signals provenance on the UI ('official' = Anthropic,
    -- 'verified' = curated by us, 'community' = third-party). Free-form
    -- text keeps the door open for new tiers without a schema change.
    trust_level text NOT NULL DEFAULT 'community',
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Partial index: GET /v1/skills/hubs returns only enabled rows on every
-- page load. enabled=false rows are rare (disabled-for-audit), so the
-- index pays for itself on every list call.
CREATE INDEX idx_skill_hubs_enabled ON skill_hubs(name) WHERE enabled = true;

-- Seed the single default we keep: Anthropic Skills. Anything else gets
-- added by hand once we decide it should ship.
INSERT INTO skill_hubs (url, name, description, trust_level) VALUES (
    'https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json',
    'Anthropic Skills',
    'Official Anthropic skill library',
    'community'
) ON CONFLICT (url) DO NOTHING;
