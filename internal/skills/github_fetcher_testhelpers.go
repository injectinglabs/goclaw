package skills

// Test helpers exported only so that other in-tree tests (e.g. the http
// package's skills_install_test.go) can swap the GitHub API + archive base
// URLs to point at an httptest.Server. Cross-package _test.go files cannot
// touch unexported package vars, hence these tiny accessor wrappers.
//
// Production code never calls these — they live in a non-_test file purely
// so the http test package can import them by their public names.

// GitHubAPIBaseForTest returns the current API base URL.
func GitHubAPIBaseForTest() string { return githubAPIBase }

// SetGitHubAPIBaseForTest overrides the API base URL.
func SetGitHubAPIBaseForTest(u string) { githubAPIBase = u }

// GitHubArchiveBaseForTest returns the current archive base URL.
func GitHubArchiveBaseForTest() string { return githubArchiveBase }

// SetGitHubArchiveBaseForTest overrides the archive base URL.
func SetGitHubArchiveBaseForTest(u string) { githubArchiveBase = u }
