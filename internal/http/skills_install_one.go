package http

import (
	"context"
	"crypto/sha256"
	"fmt"
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

// installOneSkillParams carries everything one sub-skill install needs.
// All the fields except skillRoot are constant across a bundle install —
// the dispatcher in handleInstall fetches the tarball once and reuses
// these values for every SKILL.md it finds inside.
type installOneSkillParams struct {
	ctx              context.Context // depsCtx — survives request cancellation
	r                *http.Request   // for tenant context + audit
	locale           string
	skillRoot        string // dir containing the SKILL.md to install (absolute)
	extractRoot      string // dir handed to ExtractTarball, used to derive relative path
	userID           string
	visibility       string // already validated by the dispatcher
	parentSourceURL  string // canonical source URL the user originally requested
	resolvedSHA      string
	sourceRef        string
	tenantSkillsBase string
	tenantSlug       string // for S3 mirror key
	originalSource   string // for slog only — what the user typed in
}

// installOneSkillResult is the per-sub-skill outcome. For a single-skill
// install we serialise `response` verbatim. For a bundle we accumulate
// the responses and wrap them in an envelope.
type installOneSkillResult struct {
	response map[string]any
	slug     string
	unchanged bool // true when content-hash matched — no DB write happened
}

// installOneSkillError is the typed error the dispatcher uses to know how
// to fail the HTTP request. For a single-skill install we surface it as
// the response; for a bundle we log and continue with the next sub-skill.
type installOneSkillError struct {
	statusCode int
	body       map[string]any
	err        error // optional — set when the HTTP body is a string error
}

func (e *installOneSkillError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	if msg, ok := e.body["error"].(string); ok {
		return msg
	}
	return fmt.Sprintf("install error (status %d)", e.statusCode)
}

func newInstallErr(status int, key string) *installOneSkillError {
	return &installOneSkillError{statusCode: status, body: map[string]any{"error": key}}
}

// installOneSkillFromDir runs the full install pipeline for a single
// directory that contains a SKILL.md. It is the body of the old in-line
// install handler, extracted so handleInstall can call it once per skill
// when an archive turns out to be a community-schema "bundle" (multiple
// SKILL.md under skills/<sub>/SKILL.md).
//
// The function is intentionally side-effecty in the same ways as the
// original handler (DB writes, S3 mirror goroutine, audit events) so
// nothing changes for callers that install a single skill — only the
// shape of the loop changed.
func (h *SkillsHandler) installOneSkillFromDir(p installOneSkillParams) (installOneSkillResult, *installOneSkillError) {
	// Read + validate SKILL.md.
	skillMDPath := filepath.Join(p.skillRoot, "SKILL.md")
	skillBytes, err := os.ReadFile(skillMDPath)
	if err != nil {
		return installOneSkillResult{}, newInstallErr(http.StatusBadRequest,
			i18n.T(p.locale, i18n.MsgInvalidRequest, "read SKILL.md: "+err.Error()))
	}
	skillContent := string(skillBytes)
	if strings.TrimSpace(skillContent) == "" {
		return installOneSkillResult{}, newInstallErr(http.StatusBadRequest,
			i18n.T(p.locale, i18n.MsgInvalidRequest, "SKILL.md is empty"))
	}

	violations, safe := skills.GuardSkillContent(skillContent)
	if !safe {
		slog.Warn("security.skills.install_rejected",
			"user_id", p.userID, "source", p.originalSource,
			"violations", len(violations), "first_rule", violations[0].Reason)
		return installOneSkillResult{}, &installOneSkillError{
			statusCode: http.StatusBadRequest,
			body: map[string]any{
				"error":      i18n.T(p.locale, i18n.MsgInvalidRequest, "skill content failed security scan"),
				"violations": skills.FormatGuardViolations(violations),
			},
		}
	}

	name, description, slug, frontmatter := skills.ParseSkillFrontmatter(skillContent)
	if name == "" {
		return installOneSkillResult{}, newInstallErr(http.StatusBadRequest,
			i18n.T(p.locale, i18n.MsgRequired, "name in SKILL.md frontmatter"))
	}
	if slug == "" {
		slug = skills.Slugify(name)
	}
	if !skills.SlugRegexp.MatchString(slug) {
		return installOneSkillResult{}, newInstallErr(http.StatusBadRequest,
			i18n.T(p.locale, i18n.MsgInvalidSlug, "slug"))
	}
	if h.skills.IsSystemSkill(slug) {
		return installOneSkillResult{}, newInstallErr(http.StatusConflict,
			i18n.T(p.locale, i18n.MsgInvalidRequest, "slug conflicts with a system skill"))
	}

	// Per-slug lock so two concurrent installs of the same skill (or two
	// sub-skills of the same bundle) don't race on version assignment.
	uploadLock := h.skillUploadLock(filepath.Join(p.tenantSkillsBase, slug))
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Per-user uniqueness (migration 000071): GetSkillHashBySlug now
	// scopes its lookup to (tenant, slug, owner=caller). A different
	// user in the same tenant having installed the same slug is
	// invisible here — they own a separate row. Idempotency is therefore
	// just "did THIS user install this slug before with the same SKILL.md?"
	// Re-install with identical content → unchanged. Different content →
	// upsert via ON CONFLICT (tenant, slug, owner).
	contentHash := fmt.Sprintf("%x", sha256.Sum256(skillBytes))
	existingHash, existingVer, skillExists := h.skills.GetSkillHashBySlug(p.ctx, slug)
	if skillExists && existingHash != "" && existingHash == contentHash {
		return installOneSkillResult{
			slug:      slug,
			unchanged: true,
			response: map[string]any{
				"slug":       slug,
				"version":    existingVer,
				"name":       name,
				"status":     "unchanged",
				"source_sha": p.resolvedSHA,
			},
		}, nil
	}

	version := h.skills.GetNextVersion(p.ctx, slug)
	destDir := filepath.Join(p.tenantSkillsBase, slug, fmt.Sprintf("%d", version))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return installOneSkillResult{}, newInstallErr(http.StatusInternalServerError,
			i18n.T(p.locale, i18n.MsgInternalError, "mkdir dest"))
	}

	totalSize, err := copyTreeFiltered(p.skillRoot, destDir)
	if err != nil {
		return installOneSkillResult{}, newInstallErr(http.StatusInternalServerError,
			i18n.T(p.locale, i18n.MsgInternalError, "copy: "+err.Error()))
	}

	// Derive the per-sub-skill source URL. For bundle installs, append
	// the path relative to extractRoot so update/check-updates can re-
	// fetch ONLY this sub-skill instead of the whole bundle. For root
	// installs (extractRoot == skillRoot) the parent URL is reused.
	effectiveSourceURL := p.parentSourceURL
	if relPath, _ := filepath.Rel(p.extractRoot, p.skillRoot); relPath != "" && relPath != "." {
		effectiveSourceURL = appendGitHubSubpath(p.parentSourceURL, relPath)
	}

	desc := description
	hashCopy := contentHash
	sourceURLCopy := effectiveSourceURL
	resolvedSHACopy := p.resolvedSHA
	sourceRefCopy := p.sourceRef
	installedByCopy := p.userID

	skillRow := store.SkillCreateParams{
		Name:        name,
		Slug:        slug,
		Description: &desc,
		OwnerID:     p.userID,
		Visibility:  p.visibility,
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
		"source_sha": p.resolvedSHA,
	}

	depState := uploadSkillDepState{}
	manifest := skills.ScanSkillDeps(destDir)
	if manifest != nil && !manifest.IsEmpty() {
		if ok, missing := checkUploadedSkillDeps(manifest); !ok {
			depState = h.reconcileUploadedSkillDeps(
				p.ctx, slug, manifest, missing,
				canAutoInstallUploadedSkillDeps(p.r.Context()),
			)
			skillRow.Status = depState.status
			skillRow.MissingDeps = depState.missing
			for k, v := range depState.response {
				response[k] = v
			}
		}
	}

	id, err := h.skills.CreateSkillManaged(p.ctx, skillRow)
	if err != nil {
		return installOneSkillResult{}, newInstallErr(http.StatusInternalServerError,
			i18n.T(p.locale, i18n.MsgFailedToCreate, "skill", err.Error()))
	}
	response["id"] = id

	if err := h.skills.GrantToUser(p.ctx, id, p.userID, p.userID); err != nil {
		slog.Warn("skills.install: auto-grant failed", "skill", slug, "user", p.userID, "error", err)
	}

	h.insertInstallEvent(p.ctx, slug, "installed", effectiveSourceURL, p.resolvedSHA, p.userID, map[string]any{
		"ref":     p.sourceRef,
		"version": version,
		"is_new":  isNew,
	})

	h.skills.BumpVersion()
	h.emitCacheInvalidate(bus.CacheKindSkills, id.String(), uuid.Nil)
	emitAudit(h.msgBus, p.r, "skill.installed", "skill", slug)
	depState.emit(h, slug)

	// S3 mirror — async with detached context so the response isn't held
	// hostage by upload latency. See the comment block in the dispatcher
	// for the rationale on the 5-minute deadline.
	tenantSlug := p.tenantSlug
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		h.mirrorSkillToS3(bgCtx, tenantSlug, slug, version, destDir)
	}()

	slog.Info("skill installed from source",
		"id", id, "slug", slug, "version", version,
		"source", effectiveSourceURL, "source_sha", p.resolvedSHA)

	return installOneSkillResult{response: response, slug: slug}, nil
}

// appendGitHubSubpath extends a canonical github: locator with a relative
// path segment. Used when a bundle install discovers SKILL.md files at
// skills/<sub>/SKILL.md under a parent path — each sub-skill needs its
// own source URL so a future targeted re-fetch (update / check-updates)
// can pull just that one. Best-effort: if the parent isn't a github:
// locator we fall back to returning it unchanged, which is fine because
// the only path that hits this case in production is github: tarballs.
func appendGitHubSubpath(parentURL, relPath string) string {
	relPath = filepath.ToSlash(strings.Trim(relPath, "/"))
	if relPath == "" {
		return parentURL
	}
	if !strings.HasPrefix(parentURL, "github:") {
		return parentURL
	}
	// Split at @ to keep the ref intact: github:owner/repo[/path]@ref
	atIdx := strings.LastIndex(parentURL, "@")
	if atIdx < 0 {
		return parentURL + "/" + relPath
	}
	head, tail := parentURL[:atIdx], parentURL[atIdx:]
	head = strings.TrimSuffix(head, "/")
	return head + "/" + relPath + tail
}
