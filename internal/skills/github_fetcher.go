package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Limits enforced by FetchGitHubTarball.
const (
	// MaxSkillTarballBytes caps the total bytes streamed from GitHub/upstream.
	// Independent from MaxSkillExtractedBytes — guards bandwidth/disk before
	// gunzip even runs.
	MaxSkillTarballBytes = 50 * 1024 * 1024 // 50 MB

	// SkillFetchTimeout caps a single fetch (API resolve + tarball download).
	// Applied via context.WithTimeout in FetchGitHubTarball / FetchURLTarball.
	SkillFetchTimeout = 60 * time.Second
)

// githubAPIBase is the GitHub REST endpoint used to resolve refs to commit
// SHAs. Overridden in tests to point at httptest.Server.
var githubAPIBase = "https://api.github.com"

// githubArchiveBase is the GitHub codeload tarball endpoint. Overridden in
// tests to point at httptest.Server. We split this out (separate from the API
// base) because production callers always go to github.com for /archive/.
var githubArchiveBase = "https://github.com"

// fullSHARE validates a resolved commit SHA — exactly 40 hex chars.
var fullSHARE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// FetchGitHubTarball resolves ref to a commit SHA via the GitHub REST API
// (/repos/{owner}/{repo}/commits/{ref}), then streams
// /archive/{sha}.tar.gz to a temp file under MaxSkillTarballBytes /
// SkillFetchTimeout limits.
//
// Returns the temp tarball path, the resolved 40-char SHA, and a cleanup func
// the caller MUST defer to remove the temp file.
func FetchGitHubTarball(ctx context.Context, owner, repo, ref string) (string, string, func(), error) {
	if !ghOwnerRE.MatchString(owner) || !ghRepoRE.MatchString(repo) || !ghRefRE.MatchString(ref) {
		return "", "", noopCleanup, fmt.Errorf("github_fetcher: invalid owner/repo/ref")
	}

	ctx, cancel := context.WithTimeout(ctx, SkillFetchTimeout)
	defer cancel()

	sha, err := resolveGitHubRef(ctx, owner, repo, ref)
	if err != nil {
		return "", "", noopCleanup, err
	}

	tarPath, cleanup, err := downloadGitHubArchive(ctx, owner, repo, sha)
	if err != nil {
		return "", "", noopCleanup, err
	}
	return tarPath, sha, cleanup, nil
}

// resolveGitHubRef calls GET /repos/{owner}/{repo}/commits/{ref} and pulls
// `sha` out of the JSON response. Accepts a tag, branch, or commit SHA.
func resolveGitHubRef(ctx context.Context, owner, repo, ref string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s",
		githubAPIBase,
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(ref),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: SkillFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github_fetcher: resolve ref: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("github_fetcher: ref not found: %s/%s@%s", owner, repo, ref)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("github_fetcher: resolve ref: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("github_fetcher: decode commits response: %w", err)
	}
	sha := strings.ToLower(strings.TrimSpace(out.SHA))
	if !fullSHARE.MatchString(sha) {
		return "", fmt.Errorf("github_fetcher: invalid sha from github api: %q", out.SHA)
	}
	return sha, nil
}

// downloadGitHubArchive streams /{owner}/{repo}/archive/{sha}.tar.gz to a tmp
// file, capped at MaxSkillTarballBytes.
func downloadGitHubArchive(ctx context.Context, owner, repo, sha string) (string, func(), error) {
	endpoint := fmt.Sprintf("%s/%s/%s/archive/%s.tar.gz",
		githubArchiveBase,
		url.PathEscape(owner),
		url.PathEscape(repo),
		sha,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", noopCleanup, err
	}
	req.Header.Set("Accept", "application/octet-stream")

	client := &http.Client{Timeout: SkillFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", noopCleanup, fmt.Errorf("github_fetcher: download archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", noopCleanup, fmt.Errorf("github_fetcher: archive status %d", resp.StatusCode)
	}

	return spillToTempFile(resp.Body, "goclaw-skill-tarball-*.tar.gz", MaxSkillTarballBytes)
}

// spillToTempFile copies up to maxBytes from r into a fresh temp file.
// Returns the path and a cleanup func; exceeds maxBytes => error + auto-cleanup.
func spillToTempFile(r io.Reader, pattern string, maxBytes int64) (string, func(), error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", noopCleanup, err
	}
	path := tmp.Name()
	cleanup := func() { _ = os.Remove(path) }

	// LimitReader(r, max+1) so we can detect overflow.
	n, copyErr := io.Copy(tmp, io.LimitReader(r, maxBytes+1))
	if cerr := tmp.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		cleanup()
		return "", noopCleanup, fmt.Errorf("github_fetcher: copy: %w", copyErr)
	}
	if n > maxBytes {
		cleanup()
		return "", noopCleanup, errSkillTarballTooLarge
	}
	return path, cleanup, nil
}

func noopCleanup() {}

// errSkillTarballTooLarge is returned when the streamed tarball exceeds
// MaxSkillTarballBytes. Exported only as a sentinel via errors.Is downstream.
var errSkillTarballTooLarge = errors.New("github_fetcher: tarball exceeds 50MB limit")
