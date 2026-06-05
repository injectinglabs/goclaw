package skills

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// SkillSource describes a parsed install locator pointing at either a GitHub
// repository or a direct tarball URL.
//
//   - Type == "github": Owner, Repo, Ref are populated. SHA may be filled in
//     by the fetcher once the ref resolves. Path is optional: when set
//     (e.g. "skills/pdf"), only the subdirectory of that path inside the
//     fetched tarball is treated as the skill — used for monorepos that
//     bundle multiple skills, like anthropics/skills.
//   - Type == "url": URL holds the full https://... tarball address.
//     SHA is the SHA-256 of the downloaded payload, computed by the fetcher.
type SkillSource struct {
	Type  string // "github" | "url"
	Owner string
	Repo  string
	Ref   string
	Path  string // optional subdir within the repo (github only)
	SHA   string
	URL   string
	// AmbiguousRefCandidates lists alternative (ref, path) splits the caller
	// can fall back to when the primary (Ref/Path) interpretation fails to
	// resolve at the fetcher. Populated only for HTTPS GitHub URLs of the
	// form /tree/<x>/<y>/<z> where x, x/y, x/y/z are all valid candidate
	// refs for a slash-branch — the parser commits to the most likely one
	// (greedy first-segment) but exposes the longer joins so the fetcher
	// retries them on 404 instead of giving up.
	AmbiguousRefCandidates []RefCandidate
}

// RefCandidate is one (ref, path) interpretation of an ambiguous source URL.
// `Ref` is the candidate ref string to try; `Path` is the in-repo subpath
// the caller should use as the skill root if this ref resolves.
type RefCandidate struct {
	Ref  string
	Path string
}

// Sentinel errors emitted by ParseSource.
var (
	ErrInvalidSource     = errors.New("source_locator: invalid source")
	ErrEmptySource       = errors.New("source_locator: empty source")
	ErrUnsupportedScheme = errors.New("source_locator: unsupported URL scheme (https required)")
	ErrSourceHostBlocked = errors.New("source_locator: host not in allowlist")
)

// Owner/repo identifier validation, kept conservative — GitHub allows
// alphanumeric, dashes, underscores, and dots (in repo only). Refs accept any
// non-whitespace ref-name characters up to 255 chars.
var (
	ghOwnerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	ghRepoRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	ghRefRE   = regexp.MustCompile(`^[^\s\x00]{1,255}$`)
)

// staticHostAllowlist enumerates the only hostnames a third-party install
// source may resolve to. Used by IsHostAllowed for both GitHub-style and
// generic-URL locators. Kept as a hard-coded set for Phase 1 — a future
// system_configs override can replace this without changing the API surface.
var staticHostAllowlist = map[string]bool{
	"github.com":             true,
	"raw.githubusercontent.com": true,
	"codeload.github.com":    true,
	"gitlab.com":             true,
	"bitbucket.org":          true,
}

// IsHostAllowed reports whether host (case-insensitive, no port) is on the
// allowlist. Empty host is rejected.
func IsHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	// Strip an optional :port suffix; net/url Hostname() already does this for
	// well-formed URLs, but call sites that pass raw hostnames may include one.
	if i := strings.LastIndex(host, ":"); i > -1 {
		host = host[:i]
	}
	return staticHostAllowlist[host]
}

// ParseSource parses an install locator into a SkillSource. Supported forms:
//   - github:owner/repo            → ref defaults to "main"
//   - github:owner/repo@ref        → explicit ref (tag, branch, sha)
//   - https://github.com/o/r       → ref defaults to "main"
//   - https://github.com/o/r/tree/<ref> or /commit/<sha>
//   - https://<host>/path/file.tar.gz (any allowlisted host)
//
// Anything else returns ErrInvalidSource. Host allowlist is enforced for URL
// forms; the github: scheme is implicitly trusted (github.com).
func ParseSource(input string) (SkillSource, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return SkillSource{}, ErrEmptySource
	}

	// Short scheme: github:owner/repo[@ref]
	if strings.HasPrefix(s, "github:") {
		return parseGitHubShortForm(s)
	}

	// Otherwise expect a full URL.
	u, err := url.Parse(s)
	if err != nil {
		return SkillSource{}, fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	if u.Scheme != "https" {
		return SkillSource{}, ErrUnsupportedScheme
	}
	host := strings.ToLower(u.Hostname())
	if !IsHostAllowed(host) {
		return SkillSource{}, fmt.Errorf("%w: %s", ErrSourceHostBlocked, host)
	}

	// https://github.com/owner/repo[...]
	if host == "github.com" {
		return parseGitHubURL(u)
	}

	// Fallback: treat as direct tarball URL. Require .tar.gz / .tgz extension
	// so we don't accidentally fetch HTML index pages.
	low := strings.ToLower(u.Path)
	if !strings.HasSuffix(low, ".tar.gz") && !strings.HasSuffix(low, ".tgz") {
		return SkillSource{}, fmt.Errorf("%w: expected .tar.gz URL", ErrInvalidSource)
	}
	return SkillSource{Type: "url", URL: s}, nil
}

func parseGitHubShortForm(s string) (SkillSource, error) {
	rest := strings.TrimPrefix(s, "github:")
	ref := "main"
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		ref = rest[at+1:]
		rest = rest[:at]
	}
	// Split into segments. Required: owner/repo. Optional trailing segments
	// form a subdirectory path inside the repo (e.g. "skills/pdf"), used by
	// monorepo marketplaces that bundle multiple skills.
	segments := strings.Split(rest, "/")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return SkillSource{}, fmt.Errorf("%w: expected github:owner/repo[/subdir][@ref]", ErrInvalidSource)
	}
	owner, repo := segments[0], segments[1]
	var subPath string
	if len(segments) > 2 {
		subPath = strings.Trim(strings.Join(segments[2:], "/"), "/")
	}
	if !ghOwnerRE.MatchString(owner) || !ghRepoRE.MatchString(repo) || !ghRefRE.MatchString(ref) {
		return SkillSource{}, fmt.Errorf("%w: invalid owner/repo/ref", ErrInvalidSource)
	}
	if subPath != "" {
		// Reject path traversal sequences and absolute markers in the subdir.
		if strings.Contains(subPath, "..") || strings.HasPrefix(subPath, "/") {
			return SkillSource{}, fmt.Errorf("%w: invalid subdir path", ErrInvalidSource)
		}
	}
	return SkillSource{Type: "github", Owner: owner, Repo: repo, Ref: ref, Path: subPath}, nil
}

func parseGitHubURL(u *url.URL) (SkillSource, error) {
	// Strip leading slash, split into path segments.
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return SkillSource{}, fmt.Errorf("%w: github URL missing owner/repo", ErrInvalidSource)
	}
	owner, repo := parts[0], parts[1]
	repo = strings.TrimSuffix(repo, ".git")
	if !ghOwnerRE.MatchString(owner) || !ghRepoRE.MatchString(repo) {
		return SkillSource{}, fmt.Errorf("%w: invalid github owner/repo", ErrInvalidSource)
	}
	ref := "main"
	var subPath string
	var ambiguousCandidates []RefCandidate
	// Look for /tree/<ref>/[subpath], /commit/<sha>/[subpath],
	// /releases/tag/<tag>, /archive/refs/heads/<branch>.
	if len(parts) >= 4 {
		switch parts[2] {
		case "tree", "commit":
			// GitHub's URL is genuinely ambiguous: /tree/main/foo/bar can
			// mean ref="main"+path="foo/bar" (the common monorepo case)
			// OR ref="main/foo"+path="bar" OR ref="main/foo/bar" with no
			// subpath (legal slash-branches). We commit to the first
			// interpretation as the primary — that's what GitHub's UI
			// emits when copying a subdir link — and expose the remaining
			// joins as fallback candidates so the fetcher can retry on
			// 404 without us having to call the branches API up-front.
			ref = parts[3]
			if len(parts) > 4 {
				subPath = strings.Trim(strings.Join(parts[4:], "/"), "/")
				// Tail segments after the primary ref. Each progressively
				// longer join is a fallback candidate. Order: longest →
				// shortest, because the longer joins (less specific
				// primary path) are rarer in real URLs and we want the
				// fetcher to try them before giving up.
				tail := parts[3:]
				for i := 2; i <= len(tail); i++ {
					candRef := strings.Join(tail[:i], "/")
					var candPath string
					if i < len(tail) {
						candPath = strings.Trim(strings.Join(tail[i:], "/"), "/")
					}
					ambiguousCandidates = append(ambiguousCandidates, RefCandidate{
						Ref: candRef, Path: candPath,
					})
				}
			}
		case "blob":
			// /blob/<ref>/<path>/file.md is what GitHub gives for individual
			// files. Treat the directory containing that file as the skill
			// root — same convention as `tree`, just stripping the trailing
			// file segment.
			ref = parts[3]
			if len(parts) > 5 {
				subPath = strings.Trim(strings.Join(parts[4:len(parts)-1], "/"), "/")
			}
		case "releases":
			if parts[3] == "tag" && len(parts) >= 5 {
				ref = strings.Join(parts[4:], "/")
			}
		case "archive":
			// /archive/refs/heads/<branch>.tar.gz or /archive/<sha>.tar.gz
			tail := strings.Join(parts[3:], "/")
			tail = strings.TrimSuffix(tail, ".tar.gz")
			tail = strings.TrimSuffix(tail, ".zip")
			tail = strings.TrimPrefix(tail, "refs/heads/")
			tail = strings.TrimPrefix(tail, "refs/tags/")
			if tail != "" {
				ref = tail
			}
		}
	}
	if !ghRefRE.MatchString(ref) {
		return SkillSource{}, fmt.Errorf("%w: invalid github ref", ErrInvalidSource)
	}
	if subPath != "" {
		// Same path-traversal guard as the short-form parser. Belt and braces
		// because subPath comes from a URL the user pasted, not from a hub
		// JSON we've already validated.
		if strings.Contains(subPath, "..") {
			return SkillSource{}, fmt.Errorf("%w: invalid subdir path", ErrInvalidSource)
		}
	}
	return SkillSource{
		Type:                   "github",
		Owner:                  owner,
		Repo:                   repo,
		Ref:                    ref,
		Path:                   subPath,
		AmbiguousRefCandidates: ambiguousCandidates,
	}, nil
}

// ParseMarketplaceURL extracts owner/repo/ref/baseDir from a
// raw.githubusercontent.com URL pointing at a marketplace.json. The base dir
// is the directory containing the JSON file, used to resolve relative
// in-repo paths inside the marketplace.
//
// Expected shape:
//
//	https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{...path}/marketplace.json
//
// Returns owner, repo, ref, and basePath (the dir slash-prefix; empty when
// the JSON sits at repo root).
func ParseMarketplaceURL(rawURL string) (owner, repo, ref, basePath string, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", "", "", "", fmt.Errorf("%w: %v", ErrInvalidSource, perr)
	}
	if strings.ToLower(u.Hostname()) != "raw.githubusercontent.com" {
		return "", "", "", "", fmt.Errorf("%w: expected raw.githubusercontent.com host", ErrInvalidSource)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 {
		return "", "", "", "", fmt.Errorf("%w: path too short", ErrInvalidSource)
	}
	owner, repo, ref = parts[0], parts[1], parts[2]
	if !ghOwnerRE.MatchString(owner) || !ghRepoRE.MatchString(repo) || !ghRefRE.MatchString(ref) {
		return "", "", "", "", fmt.Errorf("%w: invalid owner/repo/ref", ErrInvalidSource)
	}
	// Everything after ref except the final file segment is the base path.
	if len(parts) > 4 {
		basePath = strings.Join(parts[3:len(parts)-1], "/")
	}
	return owner, repo, ref, basePath, nil
}
