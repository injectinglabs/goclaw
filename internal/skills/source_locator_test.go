package skills

import (
	"errors"
	"testing"
)

func TestParseSource_GitHubShort(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantOwner   string
		wantRepo    string
		wantRef     string
		wantErr     error
	}{
		{name: "owner/repo defaults to main", input: "github:foo/bar", wantOwner: "foo", wantRepo: "bar", wantRef: "main"},
		{name: "owner/repo with tag", input: "github:foo/bar@v1.0.0", wantOwner: "foo", wantRepo: "bar", wantRef: "v1.0.0"},
		{name: "owner/repo with branch", input: "github:foo/bar@feature-x", wantOwner: "foo", wantRepo: "bar", wantRef: "feature-x"},
		{name: "owner/repo with sha", input: "github:foo/bar@abc1234567890abc1234567890abc1234567890", wantOwner: "foo", wantRepo: "bar", wantRef: "abc1234567890abc1234567890abc1234567890"},
		{name: "repo with dots and dashes", input: "github:open-ai/my.cool-repo", wantOwner: "open-ai", wantRepo: "my.cool-repo", wantRef: "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSource(tt.input)
			if err != nil {
				t.Fatalf("ParseSource(%q) error = %v", tt.input, err)
			}
			if got.Type != "github" {
				t.Fatalf("Type = %q, want github", got.Type)
			}
			if got.Owner != tt.wantOwner || got.Repo != tt.wantRepo || got.Ref != tt.wantRef {
				t.Fatalf("got = %+v, want owner=%s repo=%s ref=%s",
					got, tt.wantOwner, tt.wantRepo, tt.wantRef)
			}
		})
	}
}

func TestParseSource_GitHubURL(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
		wantRef   string
	}{
		{name: "bare", input: "https://github.com/foo/bar", wantOwner: "foo", wantRepo: "bar", wantRef: "main"},
		{name: "tree ref", input: "https://github.com/foo/bar/tree/v1.0", wantOwner: "foo", wantRepo: "bar", wantRef: "v1.0"},
		{name: "tree branch", input: "https://github.com/foo/bar/tree/feature/x", wantOwner: "foo", wantRepo: "bar", wantRef: "feature/x"},
		{name: "commit", input: "https://github.com/foo/bar/commit/abcdef0", wantOwner: "foo", wantRepo: "bar", wantRef: "abcdef0"},
		{name: "releases tag", input: "https://github.com/foo/bar/releases/tag/v2.3", wantOwner: "foo", wantRepo: "bar", wantRef: "v2.3"},
		{name: ".git suffix stripped", input: "https://github.com/foo/bar.git", wantOwner: "foo", wantRepo: "bar", wantRef: "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSource(tt.input)
			if err != nil {
				t.Fatalf("ParseSource(%q) error = %v", tt.input, err)
			}
			if got.Type != "github" {
				t.Fatalf("Type = %q, want github", got.Type)
			}
			if got.Owner != tt.wantOwner || got.Repo != tt.wantRepo || got.Ref != tt.wantRef {
				t.Fatalf("got = %+v, want owner=%s repo=%s ref=%s",
					got, tt.wantOwner, tt.wantRepo, tt.wantRef)
			}
		})
	}
}

func TestParseSource_DirectTarballURL(t *testing.T) {
	got, err := ParseSource("https://raw.githubusercontent.com/foo/bar/main/dist/skill.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Type != "url" {
		t.Fatalf("Type = %q, want url", got.Type)
	}
	if got.URL == "" {
		t.Fatal("URL is empty")
	}
}

func TestParseSource_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "empty", input: "", wantErr: ErrEmptySource},
		{name: "missing repo", input: "github:foo", wantErr: ErrInvalidSource},
		{name: "http scheme", input: "http://github.com/foo/bar", wantErr: ErrUnsupportedScheme},
		{name: "blocked host", input: "https://evil.com/skill.tar.gz", wantErr: ErrSourceHostBlocked},
		{name: "non-tarball url", input: "https://gitlab.com/foo/bar/index.html", wantErr: ErrInvalidSource},
		{name: "bogus", input: "not a url", wantErr: ErrUnsupportedScheme},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSource(tt.input)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want sentinel %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseSource_GitHubSubdir(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantOwn  string
		wantRepo string
		wantPath string
		wantRef  string
	}{
		{
			name: "subdir at default ref",
			input: "github:anthropics/skills/skills/pdf",
			wantOwn: "anthropics", wantRepo: "skills", wantPath: "skills/pdf", wantRef: "main",
		},
		{
			name: "subdir with explicit ref",
			input: "github:anthropics/skills/skills/pdf@main",
			wantOwn: "anthropics", wantRepo: "skills", wantPath: "skills/pdf", wantRef: "main",
		},
		{
			name: "deep nested subdir",
			input: "github:foo/bar/a/b/c/d@v1.2.3",
			wantOwn: "foo", wantRepo: "bar", wantPath: "a/b/c/d", wantRef: "v1.2.3",
		},
		{
			name: "owner/repo only — no subdir",
			input: "github:foo/bar@main",
			wantOwn: "foo", wantRepo: "bar", wantPath: "", wantRef: "main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSource(tt.input)
			if err != nil {
				t.Fatalf("ParseSource(%q) error = %v", tt.input, err)
			}
			if got.Type != "github" {
				t.Fatalf("Type = %q, want github", got.Type)
			}
			if got.Owner != tt.wantOwn || got.Repo != tt.wantRepo || got.Path != tt.wantPath || got.Ref != tt.wantRef {
				t.Fatalf("got = %+v, want owner=%s repo=%s path=%s ref=%s",
					got, tt.wantOwn, tt.wantRepo, tt.wantPath, tt.wantRef)
			}
		})
	}
}

func TestParseMarketplaceURL_Anthropic(t *testing.T) {
	owner, repo, ref, base, err := ParseMarketplaceURL(
		"https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "anthropics" || repo != "skills" || ref != "main" || base != ".claude-plugin" {
		t.Fatalf("got owner=%q repo=%q ref=%q base=%q", owner, repo, ref, base)
	}
}

func TestIsHostAllowed(t *testing.T) {
	for _, h := range []string{"github.com", "gitlab.com", "bitbucket.org", "raw.githubusercontent.com"} {
		if !IsHostAllowed(h) {
			t.Errorf("IsHostAllowed(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"", "evil.com", "github.com.evil.com", "localhost", "127.0.0.1"} {
		if IsHostAllowed(h) {
			t.Errorf("IsHostAllowed(%q) = true, want false", h)
		}
	}
}
