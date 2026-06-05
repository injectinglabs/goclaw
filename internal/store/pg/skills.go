package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGSkillStore implements store.SkillStore backed by Postgres.
// Skills metadata lives in DB; content files on filesystem.
// List queries hit PG on every call — no in-process list cache. The
// earlier per-tenant / per-(tenant,user) caches kept a version counter
// that was bumped on local mutations only; with >1 instance behind the
// ALB that left siblings serving stale lists until TTL expiry (the UI
// symptom: "I just installed/deleted a skill but it still shows the
// old state until I reload"). Postgres handles list queries well under
// realistic load — /v1/skills is a low-RPS endpoint — and a fresh
// SELECT is the simplest invariant that scales correctly to N instances
// without Redis/PG-NOTIFY plumbing.
//
// Also implements store.EmbeddingSkillSearcher for vector-based skill search.
type PGSkillStore struct {
	db      *sql.DB
	baseDir string // filesystem base for skill content
	version atomic.Int64

	// Embedding provider for vector-based skill search
	embProvider store.EmbeddingProvider
}

func NewPGSkillStore(db *sql.DB, baseDir string) *PGSkillStore {
	return &PGSkillStore{
		db:      db,
		baseDir: baseDir,
	}
}

// Version / BumpVersion remain on the interface for the filesystem-backed
// SkillStore and the long-lived seeder/watcher goroutines, which key off
// it to decide when to re-scan. Inside PGSkillStore the counter no longer
// gates anything (no list cache to invalidate), but bumps are still cheap.
func (s *PGSkillStore) Version() int64 { return s.version.Load() }
func (s *PGSkillStore) BumpVersion()   { s.version.Store(time.Now().UnixMilli()) }
func (s *PGSkillStore) Dirs() []string { return []string{s.baseDir} }

func (s *PGSkillStore) ListSkills(ctx context.Context) []store.SkillInfo {
	tid := store.TenantIDFromContext(ctx)
	if tid == uuid.Nil {
		tid = store.MasterTenantID
	}
	// Returns active + archived + system skills. Archived skills are shown dimmed in the UI
	// so admins can see missing deps and re-activate after installing them.
	// Tenant filter: system skills visible globally, custom skills scoped to tenant.
	var scanned []skillInfoRowWithFrontmatter
	if err := pkgSqlxDB.SelectContext(ctx, &scanned,
		`SELECT id, name, slug, description, visibility, tags, version, is_system, status, enabled, deps, frontmatter, file_path,
		        source_url, source_sha, source_ref, installed_by, installed_at,
		        update_available_sha, update_available_ref, last_update_check
		 FROM skills WHERE (status IN ('active', 'archived') OR is_system = true) AND (is_system = true OR tenant_id = $1)
		 ORDER BY name`, tid); err != nil {
		return nil
	}
	result := make([]store.SkillInfo, 0, len(scanned))
	for i := range scanned {
		result = append(result, scanned[i].toSkillInfo(s.baseDir))
	}
	return result
}

// ListSkillsForUser implements SkillStore. Mirrors the ListAccessible
// (skills_grants.go) visibility logic but without the per-agent dimension —
// HTTP / WS list surfaces are workspace-level, not agent-scoped. Filter:
//
//   - system skills (is_system = true)                             // global
//   - tenant.visibility = 'public'                                 // shared
//   - tenant.owner_id = $userID                                    // own
//   - EXISTS (skill_user_grants.user_id = $userID)                 // granted
//   - role IN ('owner','admin')                                    // moderation
//
// All filters rely on existing indexes: idx_skills_tenant, idx_skills_owner,
// idx_skills_visibility (partial WHERE status='active'),
// skill_user_grants(skill_id, user_id) unique. No new indexes required.
func (s *PGSkillStore) ListSkillsForUser(ctx context.Context, userID, role string) []store.SkillInfo {
	tid := store.TenantIDFromContext(ctx)
	if tid == uuid.Nil {
		tid = store.MasterTenantID
	}
	// Role used to gate an "admin sees everything in the tenant" branch
	// (including other members' private skills). That was a privacy
	// leak — a tenant owner has admin authority over the workspace, but
	// that doesn't grant clairvoyance over what each member has installed
	// privately on their own. Skills are a per-user resource just like
	// integrations: tenant_users.role only governs sharing decisions
	// (publish to team / unshare), not visibility on the inventory list.
	// Parameter kept to avoid breaking call sites.
	_ = role

	// Per-user visibility filter is the ONLY shape now. Caller sees:
	//   - system skills (global)
	//   - their own rows (owner_id matches)
	//   - tenant-public rows (admin published to team)
	//   - explicit grants (skill_user_grants)
	// Other members' private rows are invisible to everyone, including
	// the workspace owner.
	//
	// DISTINCT ON (slug) collapses duplicates that show up when both the
	// caller AND another tenant member have installed the same slug:
	// without it the UI would render "aeo" twice — once as the caller's
	// own row, once as "Shared by …". The ORDER BY picks the row to
	// keep, preferring:
	//   1. system skills (is_system desc)
	//   2. the caller's own row (owner_id = caller desc) — they can
	//      Delete/Disable/Share their copy
	//   3. shared rows (visibility='public') over granted rows
	//   4. higher version, then lexicographic id for determinism
	visibilityClause := ` AND (
		is_system = true
		OR visibility = 'public'
		OR owner_id = $2
		OR EXISTS (SELECT 1 FROM skill_user_grants g WHERE g.skill_id = skills.id AND g.user_id = $2)
	)`
	args := []any{tid, userID}

	var scanned []skillInfoRowWithFrontmatter
	if err := pkgSqlxDB.SelectContext(ctx, &scanned,
		`SELECT DISTINCT ON (slug)
		        id, name, slug, description, visibility, tags, version, is_system, status, enabled, deps, frontmatter, file_path,
		        source_url, source_sha, source_ref, installed_by, installed_at,
		        update_available_sha, update_available_ref, last_update_check
		 FROM skills
		 WHERE (status IN ('active', 'archived') OR is_system = true)
		   AND (is_system = true OR tenant_id = $1)`+visibilityClause+`
		 ORDER BY slug,
		          is_system DESC,
		          (owner_id = $2) DESC,
		          (visibility = 'public') DESC,
		          version DESC,
		          id ASC`, args...); err != nil {
		return nil
	}
	// DISTINCT ON sorts by (slug, …); a stable display order by name
	// happens here in Go to avoid wrapping the SELECT in a subquery
	// just for ORDER BY name. Few enough rows per tenant that this is
	// negligible.
	sort.Slice(scanned, func(i, j int) bool { return scanned[i].Name < scanned[j].Name })

	result := make([]store.SkillInfo, 0, len(scanned))
	for i := range scanned {
		result = append(result, scanned[i].toSkillInfo(s.baseDir))
	}

	// Per-user disable overlay (migration 72): the canonical `enabled`
	// column belongs to the row owner, but the user-facing Toggle
	// cascades by slug into skill_user_disables. The list result must
	// reflect that overlay or the SPA renders "enabled: true" for slugs
	// the caller just toggled off, with no visible change. Query once
	// per ListSkillsForUser call (cheap — index on (user_id, skill_id)
	// joined to skills.slug), collect into a set, then flip `enabled`
	// on any returned row whose slug shows up disabled.
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT sk.slug
		   FROM skill_user_disables d
		   JOIN skills sk ON sk.id = d.skill_id
		  WHERE d.user_id = $1 AND sk.tenant_id = $2`,
		userID, tid)
	if err == nil {
		disabledSlugs := map[string]struct{}{}
		for rows.Next() {
			var slug string
			if scanErr := rows.Scan(&slug); scanErr == nil {
				disabledSlugs[slug] = struct{}{}
			}
		}
		rows.Close()
		if len(disabledSlugs) > 0 {
			for i := range result {
				if _, off := disabledSlugs[result[i].Slug]; off {
					result[i].Enabled = false
				}
			}
		}
	}

	return result
}

// ListAllSkills returns system skills + custom skills for the given tenant (for admin operations like rescan-deps).
// Disabled skills are excluded — no point scanning or updating them.
func (s *PGSkillStore) ListAllSkills(ctx context.Context) []store.SkillInfo {
	tid := store.TenantIDFromContext(ctx)
	if tid == uuid.Nil {
		tid = store.MasterTenantID
	}
	var scanned []skillInfoRow
	if err := pkgSqlxDB.SelectContext(ctx, &scanned,
		`SELECT id, name, slug, description, visibility, tags, version, is_system, status, enabled, deps, file_path
		 FROM skills WHERE enabled = true AND status != 'deleted' AND (is_system = true OR tenant_id = $1)
		 ORDER BY name`, tid); err != nil {
		return nil
	}
	return skillInfoRowsToSlice(scanned, s.baseDir)
}

// ListAllSystemSkills returns only system skills (for startup dependency scanning).
// No tenant filter — system skills belong to MasterTenantID and are globally visible.
func (s *PGSkillStore) ListAllSystemSkills(ctx context.Context) []store.SkillInfo {
	var scanned []skillInfoRow
	if err := pkgSqlxDB.SelectContext(ctx, &scanned,
		`SELECT id, name, slug, description, visibility, tags, version, is_system, status, enabled, deps, file_path
		 FROM skills WHERE is_system = true AND enabled = true AND status != 'deleted'
		 ORDER BY name`); err != nil {
		return nil
	}
	return skillInfoRowsToSlice(scanned, s.baseDir)
}

// skillInfoRowsToSlice converts a slice of skillInfoRow to []store.SkillInfo. Shared by list methods.
func skillInfoRowsToSlice(rows []skillInfoRow, baseDir string) []store.SkillInfo {
	result := make([]store.SkillInfo, len(rows))
	for i := range rows {
		result[i] = rows[i].toSkillInfo(baseDir)
	}
	return result
}

// StoreMissingDeps persists the missing_deps list for a skill into the deps JSONB column.
// Works for both system and custom skills. System skills bypass tenant filter;
// custom skills require tenant_id match for cross-tenant safety.
func (s *PGSkillStore) StoreMissingDeps(ctx context.Context, id uuid.UUID, missing []string) error {
	encoded, err := marshalMissingDeps(missing)
	if err != nil {
		return err
	}
	tid := tenantIDForInsert(ctx)
	_, err = s.db.ExecContext(ctx,
		`UPDATE skills SET deps = $1, updated_at = NOW() WHERE id = $2 AND (is_system = true OR tenant_id = $3)`,
		encoded, id, tid,
	)
	if err == nil {
		s.BumpVersion()
	}
	return err
}

func marshalMissingDeps(missing []string) ([]byte, error) {
	if missing == nil {
		missing = []string{}
	}
	return json.Marshal(map[string]any{"missing": missing})
}
