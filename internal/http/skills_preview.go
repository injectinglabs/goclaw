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
}

// handlePreview fetches + extracts a remote skill, parses its SKILL.md and
// dep manifest, and returns a non-mutating preview summary. Powers the
// "Will install" confirmation step on the website. Never touches the
// managed skills directory or the DB.
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

	// Security scan is non-fatal here — we still preview, but flag warnings
	// so the UI can show a "blocked at install" banner.
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

	// Scripts listing + estimated context cost.
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

	writeJSON(w, http.StatusOK, resp)
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
