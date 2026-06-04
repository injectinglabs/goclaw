package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// stubResolveLatestSHA replaces the package-level resolveLatestSHA hook so
// tests don't have to spin up a GitHub mock for every case.
func stubResolveLatestSHA(t *testing.T, m map[string]string, fallback string) {
	t.Helper()
	orig := resolveLatestSHA
	resolveLatestSHA = func(_ context.Context, owner, repo, ref string) (string, error) {
		key := owner + "/" + repo + "@" + ref
		if v, ok := m[key]; ok {
			return v, nil
		}
		return fallback, nil
	}
	t.Cleanup(func() { resolveLatestSHA = orig })
}

// resetCheckUpdateRateLimit clears the per-tenant rate-limit state so
// concurrent tests don't stomp on each other.
func resetCheckUpdateRateLimit() {
	checkUpdateLastCall.Range(func(k, _ any) bool {
		checkUpdateLastCall.Delete(k)
		return true
	})
}

// newCheckUpdatesHandler builds a SkillsHandler with a sqlmock backing it.
func newCheckUpdatesHandler(t *testing.T) (*SkillsHandler, sqlmock.Sqlmock, *skillManageStoreStub, context.Context) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	baseDir := t.TempDir()
	skillStore := newSkillManageStoreStub(baseDir)
	h := NewSkillsHandler(skillStore, baseDir, baseDir, "", bus.New(), nil, nil)
	h.SetDB(db)

	ctx := store.WithLocale(
		store.WithTenantID(
			store.WithUserID(context.Background(), "user-1"),
			store.MasterTenantID,
		),
		"en",
	)
	return h, mock, skillStore, ctx
}

func TestCheckUpdates_DetectsNewSHA(t *testing.T) {
	resetCheckUpdateRateLimit()
	h, mock, _, ctx := newCheckUpdatesHandler(t)

	stubResolveLatestSHA(t,
		map[string]string{
			"foo/bar@main": "bbbb000000000000000000000000000000000000",
			"foo/baz@main": "cccc000000000000000000000000000000000000",
		},
		"dddd000000000000000000000000000000000000",
	)

	id1, id2, id3 := uuid.New(), uuid.New(), uuid.New()
	rows := sqlmock.NewRows([]string{"id", "slug", "source_url", "source_sha", "source_ref"}).
		AddRow(id1, "pdf", "github:foo/bar@main", "aaaa000000000000000000000000000000000000", sql.NullString{String: "main", Valid: true}).
		AddRow(id2, "csv", "github:foo/baz@main", "cccc000000000000000000000000000000000000", sql.NullString{String: "main", Valid: true}).
		AddRow(id3, "blob", "https://raw.githubusercontent.com/x/y/main.tar.gz", "00ff", sql.NullString{})

	mock.ExpectQuery(`SELECT id, slug, source_url, source_sha, source_ref\s+FROM skills`).
		WillReturnRows(rows)

	// id1 (pdf): aaaa -> bbbb (update available) → UPDATE update_available_sha
	mock.ExpectExec(`UPDATE skills\s+SET update_available_sha = \$1`).
		WithArgs("bbbb000000000000000000000000000000000000", "main", id1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// id2 (csv): cccc == cccc (no change) → clear update_available_sha
	mock.ExpectExec(`UPDATE skills\s+SET update_available_sha = NULL`).
		WithArgs(id2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// id3 (non-github): clear update_available_sha
	mock.ExpectExec(`UPDATE skills\s+SET update_available_sha = NULL`).
		WithArgs(id3).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost, "/v1/skills/check-updates", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleCheckUpdates(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp checkUpdatesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Checked != 3 {
		t.Errorf("checked = %d, want 3", resp.Checked)
	}
	if resp.UpdatesAvailable != 1 {
		t.Errorf("updates_available = %d, want 1", resp.UpdatesAvailable)
	}
	if len(resp.SkillsWithUpdates) != 1 || resp.SkillsWithUpdates[0].Slug != "pdf" {
		t.Errorf("skills_with_updates = %+v", resp.SkillsWithUpdates)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestCheckUpdates_RateLimit(t *testing.T) {
	resetCheckUpdateRateLimit()
	h, mock, _, ctx := newCheckUpdatesHandler(t)
	stubResolveLatestSHA(t, nil, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	// First call: empty skills set.
	mock.ExpectQuery(`SELECT id, slug, source_url, source_sha, source_ref\s+FROM skills`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "source_url", "source_sha", "source_ref"}))

	req := httptest.NewRequest(http.MethodPost, "/v1/skills/check-updates", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleCheckUpdates(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first call: status = %d, body = %s", w.Code, w.Body.String())
	}

	// Second call within 60s → 429.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/skills/check-updates", nil).WithContext(ctx)
	w2 := httptest.NewRecorder()
	h.handleCheckUpdates(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: status = %d (want 429), body = %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "Rate limit") {
		t.Errorf("body = %s, want Rate limit message", w2.Body.String())
	}
}

func TestSkillUpdate_AppliesNewVersion(t *testing.T) {
	resetCheckUpdateRateLimit()
	h, mock, skillStore, ctx := newCheckUpdatesHandler(t)

	// Seed the in-memory skill store with an existing skill at version 1.
	// nextBySlug must reflect the current version so GetNextVersion returns 2.
	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID:         skillID.String(),
		Name:       "PDF",
		Slug:       "pdf",
		Visibility: "private",
		Version:    1,
		Status:     "active",
		Enabled:    true,
	}
	skillStore.ownerByID[skillID] = "user-1"
	skillStore.nextBySlug["pdf"] = 1

	const (
		owner  = "ownerx"
		repo   = "skill-repo"
		newSHA = "bbbb111111111111111111111111111111111111"
	)
	srv := rawGitHubMock(t, owner, repo, newSHA, newSHA, map[string]string{
		"SKILL.md": "---\nname: PDF\nslug: pdf\n---\nupdated\n",
	})
	defer srv.Close()
	withGitHubBases(t, srv)

	// DB sees: the pending update lookup returns the recorded source_url +
	// the new SHA, then we expect an UPDATE bumping version to 2.
	mock.ExpectQuery(`SELECT source_url, update_available_sha, update_available_ref\s+FROM skills`).
		WithArgs(skillID).
		WillReturnRows(sqlmock.NewRows([]string{"source_url", "update_available_sha", "update_available_ref"}).
			AddRow("github:"+owner+"/"+repo+"@main", newSHA, sql.NullString{String: "main", Valid: true}))
	mock.ExpectExec(`UPDATE skills\s+SET version\s+= \$1`).
		WithArgs(2, newSHA, sqlmock.AnyArg(), sqlmock.AnyArg(), skillID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The insertInstallEvent helper writes an audit row.
	mock.ExpectExec(`INSERT INTO skill_install_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost, "/v1/skills/"+skillID.String()+"/update", nil).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	w := httptest.NewRecorder()
	h.handleSkillUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Version   int    `json:"version"`
		SourceSHA string `json:"source_sha"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != 2 {
		t.Errorf("version = %d, want 2", resp.Version)
	}
	if resp.SourceSHA != newSHA {
		t.Errorf("source_sha = %q, want %q", resp.SourceSHA, newSHA)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestSkillUpdate_NoUpdateAvailable(t *testing.T) {
	resetCheckUpdateRateLimit()
	h, mock, skillStore, ctx := newCheckUpdatesHandler(t)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "X", Slug: "x", Visibility: "private", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "user-1"

	mock.ExpectQuery(`SELECT source_url, update_available_sha, update_available_ref\s+FROM skills`).
		WithArgs(skillID).
		WillReturnRows(sqlmock.NewRows([]string{"source_url", "update_available_sha", "update_available_ref"}).
			AddRow(sql.NullString{String: "github:x/y@main", Valid: true}, sql.NullString{}, sql.NullString{}))

	req := httptest.NewRequest(http.MethodPost, "/v1/skills/"+skillID.String()+"/update", nil).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	w := httptest.NewRecorder()
	h.handleSkillUpdate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400), body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No update available") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestSkillUpdate_NonAdminCannotUpdatePublic(t *testing.T) {
	resetCheckUpdateRateLimit()
	// Build a handler with a tenant store that flags member-user as non-admin.
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	baseDir := t.TempDir()
	skillStore := newSkillManageStoreStub(baseDir)
	h := NewSkillsHandler(skillStore, baseDir, baseDir, "", bus.New(), nil, ts)
	h.SetDB(db)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Shared", Slug: "shared",
		Visibility: "public", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "owner-user"

	ctx := store.WithLocale(
		store.WithTenantID(
			store.WithUserID(context.Background(), "member-user"),
			tenantID,
		),
		"en",
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/skills/"+skillID.String()+"/update",
		bytes.NewReader([]byte{})).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	w := httptest.NewRecorder()
	h.handleSkillUpdate(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d (want 403), body = %s", w.Code, w.Body.String())
	}
}

// noiseRE is a sanity check pattern used by the rate-limit message assertion.
// Keeps a backreference to time.Duration formatting consistent across CI runs.
var noiseRE = regexp.MustCompile(`Rate limit: try again in \d+ seconds`)

func TestCheckUpdates_RateLimitMessageFormat(t *testing.T) {
	resetCheckUpdateRateLimit()
	h, mock, _, ctx := newCheckUpdatesHandler(t)

	mock.ExpectQuery(`SELECT id, slug, source_url, source_sha, source_ref\s+FROM skills`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "source_url", "source_sha", "source_ref"}))

	req := httptest.NewRequest(http.MethodPost, "/v1/skills/check-updates", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleCheckUpdates(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first call status = %d", w.Code)
	}

	// Immediate retry.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/skills/check-updates", nil).WithContext(ctx)
	w2 := httptest.NewRecorder()
	h.handleCheckUpdates(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d", w2.Code)
	}
	if !noiseRE.MatchString(w2.Body.String()) {
		t.Errorf("body = %q does not match expected message format", w2.Body.String())
	}
	if got := w2.Header().Get("Retry-After"); got == "" {
		t.Errorf("missing Retry-After header")
	}

	// Force the rate-limit cookie forward and confirm a subsequent call passes.
	checkUpdateLastCall.Store(store.MasterTenantID, time.Now().Add(-2*time.Minute))
	mock.ExpectQuery(`SELECT id, slug, source_url, source_sha, source_ref\s+FROM skills`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "source_url", "source_sha", "source_ref"}))
	req3 := httptest.NewRequest(http.MethodPost, "/v1/skills/check-updates", nil).WithContext(ctx)
	w3 := httptest.NewRecorder()
	h.handleCheckUpdates(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("third call status = %d, body = %s", w3.Code, w3.Body.String())
	}
}
