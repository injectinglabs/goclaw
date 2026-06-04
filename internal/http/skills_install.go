package http

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// installRequestBody is the POST /v1/skills/install payload.
type installRequestBody struct {
	Source     string `json:"source"`
	Visibility string `json:"visibility,omitempty"`
	SHA        string `json:"sha,omitempty"` // optional pin/override
}

// handleInstall installs a skill from a remote source (GitHub repo or
// direct tarball URL). Mirrors handleUpload's pipeline (parse SKILL.md →
// scan deps → copy to managed store → INSERT skill row → auto-grant to
// caller), but swaps the source: instead of a multipart ZIP upload, we
// fetch and untar a remote .tar.gz.
func (h *SkillsHandler) handleInstall(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgUserIDHeader)})
		return
	}

	var body installRequestBody
	if !bindJSON(w, r, locale, &body) {
		return
	}

	src, err := skills.ParseSource(body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, err.Error())})
		return
	}

	visibility := strings.TrimSpace(body.Visibility)
	requestedVisibility := visibility
	if visibility != "" && visibility != "public" && visibility != "private" && visibility != "internal" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "visibility must be public|private|internal")})
		return
	}

	// Role gate: writes that affect the team's shared catalog (visibility=public)
	// require owner/admin in the active tenant. Personal tenants and explicit
	// private installs are unrestricted.
	tenantID := store.TenantIDFromContext(r.Context())
	isPrivilegedWriter, err := h.isOwnerOrAdmin(r.Context(), tenantID, userID)
	if err != nil {
		slog.Warn("skills.install: role lookup failed",
			"user_id", userID, "tenant_id", tenantID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "role lookup")})
		return
	}

	if visibility == "" {
		// Default visibility: privileged callers publish team-wide, others install
		// for themselves only. This preserves the "shared catalog by default"
		// behaviour for owners/admins while keeping member installs from leaking.
		if isPrivilegedWriter {
			visibility = "public"
		} else {
			visibility = "private"
		}
	}

	if visibility == "public" && !isPrivilegedWriter {
		slog.Warn("security.skills.install_role_denied",
			"user_id", userID,
			"tenant_id", tenantID,
			"requested_visibility", requestedVisibility)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "Only owners and admins can install skills for the team. Use visibility=private to install for yourself.",
		})
		return
	}

	// 1. Fetch the tarball into a temp file.
	tarPath, resolvedSHA, fetchCleanup, sourceURL, sourceRef, err := h.fetchSkillTarball(r.Context(), src)
	if err != nil {
		slog.Warn("skills.install: fetch failed", "user_id", userID, "source", body.Source, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "fetch: "+err.Error())})
		return
	}
	defer fetchCleanup()

	// Optional caller-supplied SHA pin: reject if it disagrees with what we
	// fetched. This lets the website lock an install to a specific commit.
	if body.SHA != "" && !strings.EqualFold(body.SHA, resolvedSHA) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("sha mismatch: requested %s, resolved %s", body.SHA, resolvedSHA),
		})
		return
	}

	// 2. Extract to a tmp dir we can inspect/copy.
	tmpDir, err := os.MkdirTemp("", "goclaw-skill-install-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "tmpdir")})
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := skills.ExtractTarball(tarPath, tmpDir); err != nil {
		slog.Warn("skills.install: extract failed", "user_id", userID, "source", body.Source, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "extract: "+err.Error())})
		return
	}

	// 3. Locate SKILL.md (root, or single subdir level).
	skillRoot, err := locateSkillRoot(tmpDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, err.Error())})
		return
	}

	skillMDPath := filepath.Join(skillRoot, "SKILL.md")
	skillBytes, err := os.ReadFile(skillMDPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "read SKILL.md: "+err.Error())})
		return
	}
	skillContent := string(skillBytes)
	if strings.TrimSpace(skillContent) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "SKILL.md is empty")})
		return
	}

	// 4. Security scan SKILL.md before any disk/DB write.
	violations, safe := skills.GuardSkillContent(skillContent)
	if !safe {
		slog.Warn("security.skills.install_rejected",
			"user_id", userID,
			"source", body.Source,
			"violations", len(violations),
			"first_rule", violations[0].Reason)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      i18n.T(locale, i18n.MsgInvalidRequest, "skill content failed security scan"),
			"violations": skills.FormatGuardViolations(violations),
		})
		return
	}

	// 5. Parse frontmatter.
	name, description, slug, frontmatter := skills.ParseSkillFrontmatter(skillContent)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgRequired, "name in SKILL.md frontmatter")})
		return
	}
	if slug == "" {
		slug = skills.Slugify(name)
	}
	if !skills.SlugRegexp.MatchString(slug) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidSlug, "slug")})
		return
	}
	if h.skills.IsSystemSkill(slug) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "slug conflicts with a system skill")})
		return
	}

	// 6. Determine version + destination dir, under per-tenant slug lock.
	tenantSkillsBase := h.tenantSkillsDir(r)
	uploadLock := h.skillUploadLock(filepath.Join(tenantSkillsBase, slug))
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Content-hash idempotency: rerunning the same install with identical
	// SKILL.md returns the existing version unchanged. We use the SKILL.md
	// content hash (not the tarball hash) so mirrors / re-archived payloads
	// dedupe cleanly.
	contentHash := fmt.Sprintf("%x", sha256.Sum256(skillBytes))
	existingHash, existingVer, skillExists := h.skills.GetSkillHashBySlug(r.Context(), slug)
	if skillExists && existingHash != "" && existingHash == contentHash {
		writeJSON(w, http.StatusOK, map[string]any{
			"slug":       slug,
			"version":    existingVer,
			"name":       name,
			"status":     "unchanged",
			"source_sha": resolvedSHA,
		})
		return
	}

	version := h.skills.GetNextVersion(r.Context(), slug)
	destDir := filepath.Join(tenantSkillsBase, slug, fmt.Sprintf("%d", version))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "mkdir dest")})
		return
	}

	// 7. Copy extracted files from skillRoot → destDir.
	totalSize, err := copyTreeFiltered(skillRoot, destDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "copy: "+err.Error())})
		return
	}

	// 8. Scan deps + check.
	desc := description
	hashCopy := contentHash
	sourceURLCopy := sourceURL
	resolvedSHACopy := resolvedSHA
	sourceRefCopy := sourceRef
	installedByCopy := userID

	skill := store.SkillCreateParams{
		Name:        name,
		Slug:        slug,
		Description: &desc,
		OwnerID:     userID,
		Visibility:  visibility,
		Version:     version,
		FilePath:    destDir,
		FileSize:    totalSize,
		FileHash:    &hashCopy,
		Frontmatter: frontmatter,
		SourceURL:   &sourceURLCopy,
		SourceSHA:   &resolvedSHACopy,
		SourceRef:   &sourceRefCopy,
		InstalledBy: &installedByCopy,
	}

	isNew := !skillExists
	response := map[string]any{
		"slug":       slug,
		"version":    version,
		"name":       name,
		"status":     "active",
		"is_new":     isNew,
		"source_sha": resolvedSHA,
	}

	depState := uploadSkillDepState{}
	depsCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), uploadDepsInstallTimeout)
	defer cancel()

	manifest := skills.ScanSkillDeps(destDir)
	if manifest != nil && !manifest.IsEmpty() {
		if ok, missing := checkUploadedSkillDeps(manifest); !ok {
			depState = h.reconcileUploadedSkillDeps(
				depsCtx,
				slug,
				manifest,
				missing,
				canAutoInstallUploadedSkillDeps(r.Context()),
			)
			skill.Status = depState.status
			skill.MissingDeps = depState.missing
			for k, v := range depState.response {
				response[k] = v
			}
		}
	}

	// 9. DB write (uses non-cancellable depsCtx so disconnects don't orphan files).
	id, err := h.skills.CreateSkillManaged(depsCtx, skill)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToCreate, "skill", err.Error())})
		return
	}
	response["id"] = id

	// 10. Auto-grant to calling user so the installer immediately sees the
	// skill via ListAccessible. Best-effort — log on failure.
	if err := h.skills.GrantToUser(depsCtx, id, userID, userID); err != nil {
		slog.Warn("skills.install: auto-grant failed", "skill", slug, "user", userID, "error", err)
	}

	// 11. Audit row in skill_install_events.
	h.insertInstallEvent(depsCtx, slug, "installed", sourceURL, resolvedSHA, userID, map[string]any{
		"ref":     sourceRef,
		"version": version,
		"is_new":  isNew,
	})

	// 12. Bump cache + emit invalidate.
	h.skills.BumpVersion()
	h.emitCacheInvalidate(bus.CacheKindSkills, id.String(), uuid.Nil)
	emitAudit(h.msgBus, r, "skill.installed", "skill", slug)
	depState.emit(h, slug)

	slog.Info("skill installed from source",
		"id", id, "slug", slug, "version", version,
		"source", body.Source, "source_sha", resolvedSHA)

	writeJSON(w, http.StatusCreated, response)
}

// fetchSkillTarball resolves a SkillSource to a temp tarball path. Returns
// the resolved SHA (commit for github, sha-256 for url), a cleanup func, and
// canonical source URL / ref strings for DB storage.
func (h *SkillsHandler) fetchSkillTarball(ctx context.Context, src skills.SkillSource) (string, string, func(), string, string, error) {
	switch src.Type {
	case "github":
		tarPath, sha, cleanup, err := skills.FetchGitHubTarball(ctx, src.Owner, src.Repo, src.Ref)
		if err != nil {
			return "", "", noopCleanupFn, "", "", err
		}
		canonical := fmt.Sprintf("github:%s/%s@%s", src.Owner, src.Repo, src.Ref)
		return tarPath, sha, cleanup, canonical, src.Ref, nil
	case "url":
		tarPath, sha, cleanup, err := skills.FetchURLTarball(ctx, src.URL)
		if err != nil {
			return "", "", noopCleanupFn, "", "", err
		}
		return tarPath, sha, cleanup, src.URL, "", nil
	default:
		return "", "", noopCleanupFn, "", "", fmt.Errorf("unsupported source type %q", src.Type)
	}
}

func noopCleanupFn() {}

// locateSkillRoot returns the directory holding SKILL.md. Looks at root first,
// then descends into the single top-level subdir if root has none.
func locateSkillRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
		return dir, nil
	}
	// Look for exactly one top-level directory containing SKILL.md.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read tmpdir: %w", err)
	}
	var subdirs []string
	for _, e := range entries {
		if e.IsDir() && !skills.IsSystemArtifact(e.Name()) {
			subdirs = append(subdirs, e.Name())
		}
	}
	if len(subdirs) == 1 {
		sub := filepath.Join(dir, subdirs[0])
		if _, err := os.Stat(filepath.Join(sub, "SKILL.md")); err == nil {
			return sub, nil
		}
	}
	return "", errors.New("SKILL.md not found at archive root or in single top-level directory")
}

// copyTreeFiltered copies regular files from src to dst recursively, skipping
// system artifacts and respecting basic path-safety. Returns total bytes.
func copyTreeFiltered(src, dst string) (int64, error) {
	var total int64
	srcAbs, _ := filepath.Abs(src)
	dstAbs, _ := filepath.Abs(dst)
	err := filepath.WalkDir(srcAbs, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skills.IsSystemArtifact(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dstAbs, rel)
		if !strings.HasPrefix(target+string(os.PathSeparator), dstAbs+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path %q", rel)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(out, in)
		if closeErr := out.Close(); closeErr != nil && copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return copyErr
		}
		total += n
		return nil
	})
	return total, err
}

// insertInstallEvent writes an audit row to skill_install_events. Best-effort
// — failures are logged but don't abort the install.
func (h *SkillsHandler) insertInstallEvent(ctx context.Context, slug, eventType, sourceURL, sourceSHA, userID string, metadata map[string]any) {
	if h.db == nil {
		return
	}
	tid := store.TenantIDFromContext(ctx)
	if tid == uuid.Nil {
		tid = store.MasterTenantID
	}
	var userUUID any
	if u, err := uuid.Parse(userID); err == nil {
		userUUID = u
	}
	metaJSON := []byte("{}")
	if len(metadata) > 0 {
		if b, err := json.Marshal(metadata); err == nil {
			metaJSON = b
		}
	}
	_, err := h.db.ExecContext(ctx,
		`INSERT INTO skill_install_events (id, tenant_id, user_id, skill_slug, event_type, source_url, source_sha, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New(), tid, userUUID, slug, eventType, nullIfEmpty(sourceURL), nullIfEmpty(sourceSHA), metaJSON, time.Now().UTC(),
	)
	if err != nil {
		slog.Warn("skills.install: audit insert failed", "slug", slug, "error", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
