package http

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// checkUpdateRateLimit caps POST /v1/skills/check-updates to one call per
// tenant per 60s. Reset by the next successful call; coarse but adequate.
const checkUpdateInterval = 60 * time.Second

var (
	checkUpdateLastCall sync.Map // tenantID(uuid.UUID) -> time.Time
)

// skillUpdateInfo is one row in the check-updates response.
type skillUpdateInfo struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	CurrentSHA string `json:"current_sha"`
	LatestSHA  string `json:"latest_sha"`
	LatestRef  string `json:"latest_ref,omitempty"`
}

// checkUpdatesResponse is the body returned by POST /v1/skills/check-updates.
type checkUpdatesResponse struct {
	Checked           int               `json:"checked"`
	UpdatesAvailable  int               `json:"updates_available"`
	SkillsWithUpdates []skillUpdateInfo `json:"skills_with_updates"`
}

// handleCheckUpdates runs a per-tenant rate-limited batch check against all
// github:-sourced skills. For each skill, it resolves the configured ref
// (saved as source_ref) to its current head SHA and updates the
// update_available_* / last_update_check columns accordingly.
func (h *SkillsHandler) handleCheckUpdates(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "db not configured"})
		return
	}
	ctx := r.Context()
	tid := store.TenantIDFromContext(ctx)
	if tid == uuid.Nil {
		tid = store.MasterTenantID
	}

	// Rate limit per tenant.
	if prev, ok := checkUpdateLastCall.Load(tid); ok {
		elapsed := time.Since(prev.(time.Time))
		if elapsed < checkUpdateInterval {
			retry := checkUpdateInterval - elapsed
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retry.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": fmt.Sprintf("Rate limit: try again in %d seconds", int(retry.Seconds())+1),
			})
			return
		}
	}
	checkUpdateLastCall.Store(tid, time.Now())

	// The skills table has no `deleted_at` column — soft-delete is via
	// `status = 'deleted'`. Filtering by status keeps deleted rows out of
	// the update poll without the earlier 500 (column does not exist).
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, slug, source_url, source_sha, source_ref
		   FROM skills
		  WHERE status != 'deleted'
		    AND source_url IS NOT NULL
		    AND tenant_id = $1`, tid)
	if err != nil {
		slog.Warn("skills.check_updates: query failed", "tenant", tid, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	type checkRow struct {
		id        uuid.UUID
		slug      string
		sourceURL string
		sourceSHA sql.NullString
		sourceRef sql.NullString
	}
	var pending []checkRow
	for rows.Next() {
		var c checkRow
		if err := rows.Scan(&c.id, &c.slug, &c.sourceURL, &c.sourceSHA, &c.sourceRef); err != nil {
			slog.Warn("skills.check_updates: scan failed", "error", err)
			continue
		}
		pending = append(pending, c)
	}

	resp := checkUpdatesResponse{}
	for _, row := range pending {
		// We only know how to resolve refs for github: sources. Arbitrary URLs
		// don't carry a version concept, so skip them but still update the
		// last_update_check timestamp so the UI knows we tried.
		owner, repo, ref, isGitHub := parseGitHubSourceURL(row.sourceURL)
		if !isGitHub {
			_, _ = h.db.ExecContext(ctx,
				`UPDATE skills
				    SET update_available_sha = NULL,
				        update_available_ref = NULL,
				        last_update_check    = NOW()
				  WHERE id = $1`, row.id)
			resp.Checked++
			continue
		}

		// Prefer the recorded source_ref when present; fall back to the parsed ref.
		effectiveRef := ref
		if row.sourceRef.Valid && row.sourceRef.String != "" {
			effectiveRef = row.sourceRef.String
		}

		latestSHA, err := resolveLatestSHA(ctx, owner, repo, effectiveRef)
		if err != nil {
			slog.Warn("skills.check_updates: ref resolve failed",
				"slug", row.slug, "source", row.sourceURL, "ref", effectiveRef, "error", err)
			// Still bump last_update_check so the UI doesn't stall forever.
			_, _ = h.db.ExecContext(ctx,
				`UPDATE skills SET last_update_check = NOW() WHERE id = $1`, row.id)
			resp.Checked++
			continue
		}

		current := ""
		if row.sourceSHA.Valid {
			current = strings.ToLower(row.sourceSHA.String)
		}

		if latestSHA != current {
			_, _ = h.db.ExecContext(ctx,
				`UPDATE skills
				    SET update_available_sha = $1,
				        update_available_ref = $2,
				        last_update_check    = NOW()
				  WHERE id = $3`, latestSHA, effectiveRef, row.id)
			resp.UpdatesAvailable++
			resp.SkillsWithUpdates = append(resp.SkillsWithUpdates, skillUpdateInfo{
				ID:         row.id.String(),
				Slug:       row.slug,
				CurrentSHA: current,
				LatestSHA:  latestSHA,
				LatestRef:  effectiveRef,
			})
		} else {
			_, _ = h.db.ExecContext(ctx,
				`UPDATE skills
				    SET update_available_sha = NULL,
				        update_available_ref = NULL,
				        last_update_check    = NOW()
				  WHERE id = $1`, row.id)
		}
		resp.Checked++
	}

	// Bump cache so the new update_available_* fields propagate to the list.
	h.skills.BumpVersion()
	h.emitCacheInvalidate(bus.CacheKindSkills, "", tid)

	writeJSON(w, http.StatusOK, resp)
}

// parseGitHubSourceURL extracts owner/repo/ref from a github: locator stored
// in skills.source_url. Returns (owner, repo, ref, true) on success.
//
// Accepted forms (matching internal/skills/source_locator.go):
//   - github:owner/repo            → ref defaults to "main"
//   - github:owner/repo@ref        → explicit ref
func parseGitHubSourceURL(s string) (string, string, string, bool) {
	if !strings.HasPrefix(s, "github:") {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(s, "github:")
	ref := "main"
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		ref = rest[at+1:]
		rest = rest[:at]
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}
	return parts[0], parts[1], ref, true
}

// resolveLatestSHA wraps the package-private skills.FetchGitHubTarball ref
// resolver. We don't need the tarball here, just the SHA — but FetchGitHubTarball
// is the only exported entry, so we accept the tiny waste (it's only called
// per check-updates run, max once per minute per tenant).
//
// To avoid downloading the entire tarball for nothing, we call the same
// /repos/{owner}/{repo}/commits/{ref} endpoint directly via a small helper
// exposed by the skills package.
var resolveLatestSHA = func(ctx context.Context, owner, repo, ref string) (string, error) {
	return skills.ResolveGitHubRef(ctx, owner, repo, ref)
}

// handleSkillUpdate applies the available update for a single skill. Reuses
// the install pipeline's fetch + extract + copy stages, then bumps version,
// clears update_available_*, and logs an audit event.
func (h *SkillsHandler) handleSkillUpdate(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "db not configured"})
		return
	}
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgUserIDHeader)})
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "skill")})
		return
	}

	sk, found := h.skills.GetSkillByID(r.Context(), id)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgNotFound, "skill", idStr)})
		return
	}

	// Role gate: same rule as install — public-skill writes are owner/admin only.
	if sk.Visibility == "public" {
		tenantID := store.TenantIDFromContext(r.Context())
		ok, err := h.isOwnerOrAdmin(r.Context(), tenantID, userID)
		if err != nil {
			slog.Warn("skills.update_apply: role lookup failed",
				"skill_id", idStr, "user_id", userID, "tenant_id", tenantID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "role lookup")})
			return
		}
		if !ok {
			slog.Warn("security.skills.update_role_denied",
				"skill_id", idStr, "user_id", userID, "tenant_id", tenantID)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "Only owners and admins can install skills for the team. Use visibility=private to install for yourself.",
			})
			return
		}
	}

	// Pull pending update from DB.
	var (
		sourceURL        sql.NullString
		updateAvailable  sql.NullString
		updateAvailRef   sql.NullString
	)
	err = h.db.QueryRowContext(r.Context(),
		`SELECT source_url, update_available_sha, update_available_ref
		   FROM skills WHERE id = $1`, id).Scan(&sourceURL, &updateAvailable, &updateAvailRef)
	if err != nil {
		slog.Warn("skills.update_apply: row lookup failed", "skill_id", idStr, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if !updateAvailable.Valid || updateAvailable.String == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No update available"})
		return
	}
	if !sourceURL.Valid || !strings.HasPrefix(sourceURL.String, "github:") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "skill has no github: source — update not supported"})
		return
	}

	owner, repo, _, isGitHub := parseGitHubSourceURL(sourceURL.String)
	if !isGitHub {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid github source"})
		return
	}
	newSHA := strings.ToLower(updateAvailable.String)
	newRef := ""
	if updateAvailRef.Valid {
		newRef = updateAvailRef.String
	}

	// Fetch + extract the new tarball.
	tarPath, resolvedSHA, cleanup, err := skills.FetchGitHubTarball(r.Context(), owner, repo, newSHA)
	if err != nil {
		slog.Warn("skills.update_apply: fetch failed",
			"skill", sk.Slug, "owner", owner, "repo", repo, "sha", newSHA, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetch: " + err.Error()})
		return
	}
	defer cleanup()

	tmpDir, err := os.MkdirTemp("", "goclaw-skill-update-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tmpdir"})
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := skills.ExtractTarball(tarPath, tmpDir); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "extract: " + err.Error()})
		return
	}

	skillRoot, err := locateSkillRoot(tmpDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	skillMDPath := filepath.Join(skillRoot, "SKILL.md")
	skillBytes, err := os.ReadFile(skillMDPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read SKILL.md: " + err.Error()})
		return
	}
	if violations, safe := skills.GuardSkillContent(string(skillBytes)); !safe {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      "skill content failed security scan",
			"violations": skills.FormatGuardViolations(violations),
		})
		return
	}

	// Stage the new version dir.
	tenantSkillsBase := h.tenantSkillsDir(r)
	uploadLock := h.skillUploadLock(filepath.Join(tenantSkillsBase, sk.Slug))
	uploadLock.Lock()
	defer uploadLock.Unlock()

	newVersion := h.skills.GetNextVersion(r.Context(), sk.Slug)
	destDir := filepath.Join(tenantSkillsBase, sk.Slug, fmt.Sprintf("%d", newVersion))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "mkdir dest"})
		return
	}
	if _, err := copyTreeFiltered(skillRoot, destDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "copy: " + err.Error()})
		return
	}

	// Update the row: bump version + sha, clear pending flags, refresh check time.
	now := time.Now().UTC()
	if _, err := h.db.ExecContext(r.Context(),
		`UPDATE skills
		    SET version              = $1,
		        source_sha           = $2,
		        update_available_sha = NULL,
		        update_available_ref = NULL,
		        last_update_check    = $3,
		        file_path            = $4,
		        updated_at           = $3
		  WHERE id = $5`,
		newVersion, resolvedSHA, now, destDir, id); err != nil {
		slog.Warn("skills.update_apply: update row failed", "skill", sk.Slug, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update row: " + err.Error()})
		return
	}

	// Audit row.
	meta := map[string]any{
		"old_version": sk.Version,
		"new_version": newVersion,
		"ref":         newRef,
	}
	h.insertInstallEvent(r.Context(), sk.Slug, "updated", sourceURL.String, resolvedSHA, userID, meta)

	// Bump skill loader cache so the new version is picked up.
	h.skills.BumpVersion()
	h.emitCacheInvalidate(bus.CacheKindSkills, id.String(), uuid.Nil)
	emitAudit(h.msgBus, r, "skill.updated", "skill", sk.Slug)

	// Mirror the new version to S3. See skills_install.go for the same
	// pattern (detached background context + 5-minute deadline).
	tenantSlugForMirror := store.TenantSlugFromContext(r.Context())
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		h.mirrorSkillToS3(bgCtx, tenantSlugForMirror, sk.Slug, newVersion, destDir)
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id.String(),
		"slug":       sk.Slug,
		"version":    newVersion,
		"source_sha": resolvedSHA,
		"status":     "updated",
	})
}

