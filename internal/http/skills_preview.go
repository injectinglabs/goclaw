package http

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// previewRequestBody mirrors installRequestBody minus visibility — preview
// never writes anything, so visibility doesn't apply.
type previewRequestBody struct {
	Source string `json:"source"`
}

// previewScriptInfo describes a single script file under scripts/.
type previewScriptInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// previewDeps groups deps from the manifest into the three runtimes the
// website cares about (python/node/system).
type previewDeps struct {
	Python []string `json:"python"`
	Node   []string `json:"node"`
	System []string `json:"system"`
}

// previewResponse is the JSON body returned by POST /v1/skills/preview.
//
// BundleCount / BundleSlugs are populated only when the resolved source
// expands to more than one skill (e.g. `github:anthropics/skills`, where
// the archive contains skills/<sub>/SKILL.md for each entry). In that
// case the main fields (slug, name, deps, scripts, estimated_chars)
// describe the FIRST sub-skill — estimated_chars is the SUM across all
// sub-skills so the user sees the total context cost. BundleSlugs lists
// every sub-skill that will be installed so the UI can render "+N more".
//
// Backward compatible: pre-bundle SPAs ignore the extra fields and keep
// rendering a single-skill preview, which still works because install is
// bundle-aware and processes the full set.
type previewResponse struct {
	Slug           string              `json:"slug"`
	Name           string              `json:"name"`
	Description    string              `json:"description"`
	VersionString  string              `json:"version_string"`
	Scripts        []previewScriptInfo `json:"scripts"`
	Deps           previewDeps         `json:"deps"`
	EstimatedChars int64               `json:"estimated_chars"`
	SourceSHA      string              `json:"source_sha"`
	Warnings       []string            `json:"warnings"`
	BundleCount    int                 `json:"bundle_count,omitempty"`
	BundleSlugs    []string            `json:"bundle_slugs,omitempty"`
}

// handlePreview fetches + extracts a remote skill, parses its SKILL.md and
// dep manifest, and returns a non-mutating preview summary. Powers the
// "Will install" confirmation step on the website. Never touches the
// managed skills directory or the DB.
//
// Supports both single-skill sources and bundle sources (Anthropic-style
// monorepo containing multiple SKILL.md files). Detection mirrors
// handleInstall: locateSkillRoot first, locateBundleSkillDirs as fallback.
func (h *SkillsHandler) handlePreview(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgUserIDHeader)})
		return
	}

	var body previewRequestBody
	if !bindJSON(w, r, locale, &body) {
		return
	}

	src, err := skills.ParseSource(body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, err.Error())})
		return
	}

	tarPath, resolvedSHA, fetchCleanup, _, sourceRef, err := h.fetchSkillTarball(r.Context(), src)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "fetch: "+err.Error())})
		return
	}
	defer fetchCleanup()
	_ = sourceRef // reserved for warnings if ref vs SHA diverge — not surfaced yet

	tmpDir, err := os.MkdirTemp("", "goclaw-skill-preview-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "tmpdir")})
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := skills.ExtractTarballSubdir(tarPath, tmpDir, src.Path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "extract: "+err.Error())})
		return
	}

	// Same discovery as install: try single-skill root first, fall back to
	// bundle layout. Without this branch the preview rejected every monorepo
	// source while install happily handled it — a confusing UX gap.
	var skillRoots []string
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
	}

	// Build per-skill previews. Sort by slug so the "first" skill the user
	// sees is deterministic across reruns — matters for the bundle case
	// where the displayed name otherwise jumps with filesystem iteration
	// order.
	previews := make([]previewResponse, 0, len(skillRoots))
	for _, root := range skillRoots {
		p, perr := h.buildSkillPreview(root, resolvedSHA)
		if perr != nil {
			// Per-sub-skill preview failure in a bundle is non-fatal; skip
			// the broken sub-skill so the user still sees the rest. For a
			// single-skill source this is the same as the old behaviour
			// (HTTP 400 with the reason).
			if len(skillRoots) == 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": i18n.T(locale, i18n.MsgInvalidRequest, perr.Error()),
				})
				return
			}
			continue
		}
		previews = append(previews, p)
	}

	if len(previews) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": i18n.T(locale, i18n.MsgInvalidRequest, "no readable SKILL.md found in bundle"),
		})
		return
	}

	sort.Slice(previews, func(i, j int) bool { return previews[i].Slug < previews[j].Slug })

	// Single-skill response keeps the historic shape exactly — no bundle
	// fields, scripts/deps/chars belong to that one skill.
	if len(previews) == 1 {
		writeJSON(w, http.StatusOK, previews[0])
		return
	}

	// Bundle response: surface the first skill's details (so the existing
	// SPA renders something useful) but overwrite estimated_chars with the
	// aggregate so the cost line reflects what the user actually pays.
	primary := previews[0]
	var totalChars int64
	slugs := make([]string, 0, len(previews))
	for _, p := range previews {
		totalChars += p.EstimatedChars
		if p.Slug != "" {
			slugs = append(slugs, p.Slug)
		}
	}
	primary.EstimatedChars = totalChars
	primary.BundleCount = len(previews)
	primary.BundleSlugs = slugs
	writeJSON(w, http.StatusOK, primary)
}

// buildSkillPreview produces a previewResponse for one skill directory.
// Returns the assembled response and a non-nil error only when SKILL.md
// is missing or unreadable (the cases that legitimately abort preview).
// Soft problems — security warnings, missing frontmatter — go into
// resp.Warnings so the UI can display them without blocking install.
func (h *SkillsHandler) buildSkillPreview(skillRoot, resolvedSHA string) (previewResponse, error) {
	skillMDPath := filepath.Join(skillRoot, "SKILL.md")
	skillBytes, err := os.ReadFile(skillMDPath)
	if err != nil {
		return previewResponse{}, err
	}
	skillContent := string(skillBytes)

	name, description, slug, frontmatter := skills.ParseSkillFrontmatter(skillContent)
	if slug == "" {
		slug = skills.Slugify(name)
	}

	resp := previewResponse{
		Slug:          slug,
		Name:          name,
		Description:   description,
		VersionString: frontmatter["version"],
		SourceSHA:     resolvedSHA,
		Scripts:       []previewScriptInfo{},
		Deps:          previewDeps{Python: []string{}, Node: []string{}, System: []string{}},
		Warnings:      []string{},
	}

	// Security scan is non-fatal — install will block, but preview should
	// surface the warning so the user knows ahead of time.
	if violations, safe := skills.GuardSkillContent(skillContent); !safe {
		for _, v := range violations {
			resp.Warnings = append(resp.Warnings, v.Reason)
		}
	}
	if name == "" {
		resp.Warnings = append(resp.Warnings, "missing `name` in SKILL.md frontmatter — install will fail")
	}
	if slug != "" && !skills.SlugRegexp.MatchString(slug) {
		resp.Warnings = append(resp.Warnings, "invalid slug: "+slug)
	}
	if h.skills.IsSystemSkill(slug) {
		resp.Warnings = append(resp.Warnings, "slug conflicts with a system skill — install will fail")
	}

	resp.EstimatedChars = int64(len(skillBytes))
	scriptsDir := filepath.Join(skillRoot, "scripts")
	if entries, _ := os.ReadDir(scriptsDir); len(entries) > 0 {
		walkScriptsDir(scriptsDir, &resp)
	}
	sort.Slice(resp.Scripts, func(i, j int) bool { return resp.Scripts[i].Name < resp.Scripts[j].Name })

	if manifest := skills.ScanSkillDeps(skillRoot); manifest != nil {
		resp.Deps.Python = append([]string{}, manifest.RequiresPython...)
		resp.Deps.Node = append([]string{}, manifest.RequiresNode...)
		resp.Deps.System = append([]string{}, manifest.Requires...)
	}

	return resp, nil
}

// walkScriptsDir walks the scripts/ directory and appends entries to
// resp.Scripts + adds their sizes to EstimatedChars.
func walkScriptsDir(scriptsDir string, resp *previewResponse) {
	_ = filepath.WalkDir(scriptsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(scriptsDir, path)
		if relErr != nil {
			return nil
		}
		if skills.IsSystemArtifact(rel) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		// Use forward-slash relative path so frontend sees stable separators.
		resp.Scripts = append(resp.Scripts, previewScriptInfo{
			Name: strings.ReplaceAll(rel, string(os.PathSeparator), "/"),
			Size: info.Size(),
		})
		resp.EstimatedChars += info.Size()
		return nil
	})
}
