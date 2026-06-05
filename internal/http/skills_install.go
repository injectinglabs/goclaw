package http

import (
	"context"
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

	// 1. Fetch the tarball into a temp file. Pass &src so the fetcher can
	// rewrite Ref/Path if it had to fall back to one of the parser's
	// AmbiguousRefCandidates — downstream code reads src.Path for the
	// tarball-subdir extract and needs the resolved value.
	tarPath, resolvedSHA, fetchCleanup, sourceURL, sourceRef, err := h.fetchSkillTarball(r.Context(), &src)
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

	if err := skills.ExtractTarballSubdir(tarPath, tmpDir, src.Path); err != nil {
		slog.Warn("skills.install: extract failed", "user_id", userID, "source", body.Source, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "extract: "+err.Error())})
		return
	}

	// 3. Discover SKILL.md location(s). Two paths:
	//    - Single skill (Anthropic-style): SKILL.md at archive root or in a
	//      single top-level dir. locateSkillRoot returns it directly.
	//    - Bundle (community-style): the archive is a container with
	//      skills/<sub>/SKILL.md inside. locateBundleSkillDirs walks the
	//      tree up to 4 levels deep and returns each leaf containing a
	//      SKILL.md. We loop the per-skill install in that case.
	//
	// Depth cap 4 lets us catch the realistic "marketing-skill/skills/foo/
	// SKILL.md" layout without mining vendor trees.
	skillRoots, bundle := []string{}, false
	if root, lerr := locateSkillRoot(tmpDir); lerr == nil {
		skillRoots = []string{root}
	} else {
		bundleDirs, _ := locateBundleSkillDirs(tmpDir, 4)
		if len(bundleDirs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": i18n.T(locale, i18n.MsgInvalidRequest, lerr.Error()),
			})
			return
		}
		skillRoots = bundleDirs
		bundle = len(bundleDirs) > 1
	}

	// Shared deps context — survives request cancellation so an HTTP
	// timeout/disconnect doesn't orphan partially-written DB rows.
	depsCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), uploadDepsInstallTimeout)
	defer cancel()

	tenantSkillsBase := h.tenantSkillsDir(r)
	tenantSlugForMirror := store.TenantSlugFromContext(r.Context())

	// 4. Install each discovered skill. For the single-skill path we
	// surface any failure as the HTTP response so the existing client
	// contract is preserved. For a bundle we log per-skill failures and
	// keep going — half a bundle is better than no bundle, and the user
	// can re-install any missed sub-skills explicitly.
	installed := make([]map[string]any, 0, len(skillRoots))
	var bundleFirstErr *installOneSkillError
	for _, skillRoot := range skillRoots {
		res, ierr := h.installOneSkillFromDir(installOneSkillParams{
			ctx:              depsCtx,
			r:                r,
			locale:           locale,
			skillRoot:        skillRoot,
			extractRoot:      tmpDir,
			userID:           userID,
			visibility:       visibility,
			parentSourceURL:  sourceURL,
			resolvedSHA:      resolvedSHA,
			sourceRef:        sourceRef,
			tenantSkillsBase: tenantSkillsBase,
			tenantSlug:       tenantSlugForMirror,
			originalSource:   body.Source,
		})
		if ierr != nil {
			if !bundle {
				writeJSON(w, ierr.statusCode, ierr.body)
				return
			}
			slog.Warn("skills.install.bundle.sub_install_failed",
				"skill_root", skillRoot, "error", ierr.Error())
			if bundleFirstErr == nil {
				bundleFirstErr = ierr
			}
			continue
		}
		installed = append(installed, res.response)
	}

	// 5. Respond. Single-skill keeps the historical shape; bundle returns
	// a wrapped envelope so the SPA can show "Installed N skills" toast.
	if !bundle {
		if len(installed) > 0 {
			// Idempotent reruns (status=unchanged) use 200; new installs
			// use 201. Both shapes are unchanged from the pre-refactor API.
			status := http.StatusCreated
			if s, _ := installed[0]["status"].(string); s == "unchanged" {
				status = http.StatusOK
			}
			writeJSON(w, status, installed[0])
		}
		return
	}
	if len(installed) == 0 {
		// Whole bundle failed. Surface the first error so the user has
		// something actionable, not a generic "0 skills installed".
		body := map[string]any{"error": "bundle install: no sub-skills could be installed"}
		if bundleFirstErr != nil {
			body["first_error"] = bundleFirstErr.Error()
		}
		writeJSON(w, http.StatusBadRequest, body)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"bundle":    true,
		"count":     len(installed),
		"installed": installed,
	})
}

// fetchSkillTarball resolves a SkillSource to a temp tarball path. Returns
// the resolved SHA (commit for github, sha-256 for url), a cleanup func, and
// canonical source URL / ref strings for DB storage.
//
// For HTTPS GitHub URLs of the form /tree/<x>/<y>/<z> the parser commits to
// a single (Ref, Path) split as primary but exposes AmbiguousRefCandidates
// with the alternative joins (longer refs / shorter paths). When the primary
// 404s at GitHub we walk the candidate list and retry — this is how we
// support both monorepo subdir links (the common case) and slash-branches
// like `feature/x` without hardcoding branch-name heuristics. The mutator
// updates src.Ref / src.Path in place so the caller's extract step uses the
// path that actually resolved.
func (h *SkillsHandler) fetchSkillTarball(ctx context.Context, src *skills.SkillSource) (string, string, func(), string, string, error) {
	switch src.Type {
	case "github":
		tarPath, sha, cleanup, err := skills.FetchGitHubTarball(ctx, src.Owner, src.Repo, src.Ref)
		if err != nil && isGitHubRefNotFound(err) {
			// Walk candidates: each is a (longer ref, shorter path) split of
			// the URL tail. Stop at the first success.
			for _, cand := range src.AmbiguousRefCandidates {
				slog.Info("skills.install: retrying github fetch with alternate ref",
					"primary_ref", src.Ref, "candidate_ref", cand.Ref, "candidate_path", cand.Path)
				if p, s, c, e := skills.FetchGitHubTarball(ctx, src.Owner, src.Repo, cand.Ref); e == nil {
					tarPath, sha, cleanup, err = p, s, c, nil
					src.Ref = cand.Ref
					src.Path = cand.Path
					break
				}
			}
		}
		if err != nil {
			return "", "", noopCleanupFn, "", "", err
		}
		canonical := fmt.Sprintf("github:%s/%s@%s", src.Owner, src.Repo, src.Ref)
		if src.Path != "" {
			canonical = fmt.Sprintf("github:%s/%s/%s@%s", src.Owner, src.Repo, src.Path, src.Ref)
		}
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

// isGitHubRefNotFound reports whether an error from FetchGitHubTarball is
// the "ref doesn't exist" case (HTTP 404 at the refs resolver). Used to
// gate the alternate-candidate retry — we don't want to retry on, say, a
// timeout, because the candidate would hit the same timeout.
func isGitHubRefNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "github_fetcher: ref not found")
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

// locateBundleSkillDirs walks dir up to maxDepth levels looking for every
// directory that contains a SKILL.md. Used when the archive is a community
// "plugin" — a container whose actual skill files live deeper, typically
// under skills/<sub-name>/SKILL.md or <sub-name>/SKILL.md.
//
// Returned paths are absolute and exclude any directory locateSkillRoot
// would already have caught — the bundle path is only meaningful when the
// straightforward lookup failed. Order is lexicographic so a bundle install
// always produces a deterministic skill order in the response.
func locateBundleSkillDirs(dir string, maxDepth int) ([]string, error) {
	rootAbs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var found []string
	err = filepath.WalkDir(rootAbs, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Skip system/hidden artifacts (.git, __MACOSX, node_modules, …)
			if p != rootAbs && skills.IsSystemArtifact(d.Name()) {
				return filepath.SkipDir
			}
			// Depth cap: avoid mining N-level vendor trees by accident.
			rel, _ := filepath.Rel(rootAbs, p)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "SKILL.md" {
			found = append(found, filepath.Dir(p))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
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
