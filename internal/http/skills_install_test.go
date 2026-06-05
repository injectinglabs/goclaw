package http

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/skills"
)

// rawGitHubMock returns an httptest.Server that imitates:
//   - GET /repos/{owner}/{repo}/commits/{ref} → {"sha": fakeSHA}
//   - GET /{owner}/{repo}/archive/{sha}.tar.gz → gzipped tar payload
//
// It honors the standard GitHub-style top-level wrapper directory.
func rawGitHubMock(t *testing.T, owner, repo, ref, sha string, files map[string]string) *httptest.Server {
	t.Helper()
	wrapper := repo + "-" + sha[:7] + "/"

	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     wrapper + name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	tarBytes := tarBuf.Bytes()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/"+ref, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
	})
	mux.HandleFunc("/"+owner+"/"+repo+"/archive/"+sha+".tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarBytes)
	})
	return httptest.NewServer(mux)
}

// withGitHubBases swaps the package-level GitHub API + archive base URLs to
// point at the test server, restoring originals on cleanup.
func withGitHubBases(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origAPI := getGitHubAPIBase()
	origArchive := getGitHubArchiveBase()
	setGitHubAPIBase(srv.URL)
	setGitHubArchiveBase(srv.URL)
	t.Cleanup(func() {
		setGitHubAPIBase(origAPI)
		setGitHubArchiveBase(origArchive)
	})
}

// Test that the install handler fetches → extracts → creates the skill row.
func TestSkillInstall_HappyPath(t *testing.T) {
	const (
		owner = "foo"
		repo  = "bar"
		ref   = "main"
		sha   = "0123456789abcdef0123456789abcdef01234567"
	)

	skillMD := "---\nname: GH Skill\nslug: gh-skill\n---\nA test skill\n"
	srv := rawGitHubMock(t, owner, repo, ref, sha, map[string]string{
		"SKILL.md":         skillMD,
		"scripts/main.py":  "print('hi')\n",
		"README.md":        "doc",
	})
	defer srv.Close()
	withGitHubBases(t, srv)

	handler, skillStore, ctx, _ := newTestUploadHandler(t)
	stubUploadDepFns(t,
		func(context.Context, *skills.SkillManifest, []string) (*skills.InstallResult, error) {
			return nil, nil
		},
		func(*skills.SkillManifest) (bool, []string) { return true, nil },
	)

	body, _ := json.Marshal(map[string]string{
		"source":     "github:" + owner + "/" + repo + "@" + ref,
		"visibility": "private",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.handleInstall(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		ID        string `json:"id"`
		Slug      string `json:"slug"`
		Version   int    `json:"version"`
		Status    string `json:"status"`
		SourceSHA string `json:"source_sha"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Slug != "gh-skill" {
		t.Errorf("slug = %q, want gh-skill", resp.Slug)
	}
	if resp.SourceSHA != sha {
		t.Errorf("source_sha = %q, want %q", resp.SourceSHA, sha)
	}
	if resp.Status != "active" {
		t.Errorf("status = %q, want active", resp.Status)
	}
	if resp.ID == "" {
		t.Error("id is empty")
	}

	// Verify skill row exists in the stub store.
	if got := skillStore.nextBySlug["gh-skill"]; got != 1 {
		t.Errorf("nextBySlug = %d, want 1", got)
	}
}

// Test that a tar containing a path-traversal entry is rejected before write.
func TestSkillInstall_PathTraversalRejected(t *testing.T) {
	const (
		owner = "foo"
		repo  = "evil"
		ref   = "main"
		sha   = "feedfacefeedfacefeedfacefeedfacefeedface"
	)
	srv := rawGitHubMock(t, owner, repo, ref, sha, map[string]string{
		"SKILL.md":            "---\nname: Evil\nslug: evil\n---\nbody\n",
		"../../etc/passwd":    "evil", // wrapper prefix in mock will still prepend, but raw "..." in tar header is what triggers
	})
	defer srv.Close()
	withGitHubBases(t, srv)

	handler, _, ctx, _ := newTestUploadHandler(t)
	body, _ := json.Marshal(map[string]string{
		"source": "github:" + owner + "/" + repo + "@" + ref,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.handleInstall(w, req)

	// The wrapper-prefix mock prepends `{repo}-{sha7}/` so the path becomes
	// `evil-feedfac/../../etc/passwd` — pathTraversalRE catches the `../`.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s (expected 400)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "extract") && !strings.Contains(w.Body.String(), "traversal") {
		t.Errorf("body = %s, expected extract/traversal error", w.Body.String())
	}
}

// Test that the install handler refuses invalid source strings up front.
func TestSkillInstall_RejectsInvalidSource(t *testing.T) {
	handler, _, ctx, _ := newTestUploadHandler(t)
	body, _ := json.Marshal(map[string]string{"source": "ftp://evil.com/x.tar.gz"})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.handleInstall(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

// Test that a SHA pin mismatch is rejected.
func TestSkillInstall_SHAPinMismatch(t *testing.T) {
	const (
		owner = "foo"
		repo  = "bar"
		ref   = "main"
		sha   = "1111111111111111111111111111111111111111"
	)
	srv := rawGitHubMock(t, owner, repo, ref, sha, map[string]string{
		"SKILL.md": "---\nname: Pinned\nslug: pinned\n---\n",
	})
	defer srv.Close()
	withGitHubBases(t, srv)

	handler, _, ctx, _ := newTestUploadHandler(t)
	body, _ := json.Marshal(map[string]string{
		"source": "github:" + owner + "/" + repo + "@" + ref,
		"sha":    "2222222222222222222222222222222222222222",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.handleInstall(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sha mismatch") {
		t.Errorf("expected sha mismatch error, got %s", w.Body.String())
	}
}

// Test that POST /v1/skills/preview returns a non-mutating summary.
func TestSkillPreview_HappyPath(t *testing.T) {
	const (
		owner = "foo"
		repo  = "previewme"
		ref   = "main"
		sha   = "0badf00d0badf00d0badf00d0badf00d0badf00d"
	)
	srv := rawGitHubMock(t, owner, repo, ref, sha, map[string]string{
		"SKILL.md":         "---\nname: Preview Skill\nslug: preview-skill\nversion: 1.2.3\n---\nA preview\n",
		"scripts/run.py":   "print('hello')\n",
	})
	defer srv.Close()
	withGitHubBases(t, srv)

	handler, skillStore, ctx, _ := newTestUploadHandler(t)
	body, _ := json.Marshal(map[string]string{
		"source": "github:" + owner + "/" + repo + "@" + ref,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/preview", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.handlePreview(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp previewResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Slug != "preview-skill" {
		t.Errorf("slug = %q, want preview-skill", resp.Slug)
	}
	if resp.VersionString != "1.2.3" {
		t.Errorf("version_string = %q, want 1.2.3", resp.VersionString)
	}
	if resp.SourceSHA != sha {
		t.Errorf("source_sha = %q, want %q", resp.SourceSHA, sha)
	}
	if len(resp.Scripts) != 1 || resp.Scripts[0].Name != "run.py" {
		t.Errorf("scripts = %#v, want one run.py entry", resp.Scripts)
	}
	if resp.EstimatedChars == 0 {
		t.Errorf("estimated_chars = 0, want > 0")
	}

	// Preview must NOT mutate the store.
	if len(skillStore.skills) != 0 {
		t.Errorf("preview mutated store: %d skills present", len(skillStore.skills))
	}
}

// --- Test helpers to override package-private base URLs ---

func getGitHubAPIBase() string         { return skills.GitHubAPIBaseForTest() }
func setGitHubAPIBase(u string)        { skills.SetGitHubAPIBaseForTest(u) }
func getGitHubArchiveBase() string     { return skills.GitHubArchiveBaseForTest() }
func setGitHubArchiveBase(u string)    { skills.SetGitHubArchiveBaseForTest(u) }

