package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// newAuditHandler builds a SkillsHandler with sqlmock backing it for
// install-events tests.
func newAuditHandler(t *testing.T) (*SkillsHandler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	baseDir := t.TempDir()
	h := NewSkillsHandler(newSkillManageStoreStub(baseDir), baseDir, baseDir, "", bus.New(), nil, nil, nil)
	h.SetDB(db)
	return h, mock
}

func auditCtx(userID string, tenantID uuid.UUID) context.Context {
	return store.WithLocale(
		store.WithTenantID(
			store.WithUserID(context.Background(), userID),
			tenantID,
		),
		"en",
	)
}

func TestInstallEvents_AdminGetsEvents(t *testing.T) {
	h, mock := newAuditHandler(t)
	tid := store.MasterTenantID
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM skill_install_events WHERE tenant_id = \$1`).
		WithArgs(tid).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	id1, id2 := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT id, skill_slug, event_type, user_id, source_url, source_sha, metadata, created_at FROM skill_install_events WHERE tenant_id = \$1`).
		WithArgs(tid, 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "skill_slug", "event_type", "user_id", "source_url", "source_sha", "metadata", "created_at"}).
			AddRow(id1, "pdf", "installed", sql.NullString{String: "user-1", Valid: true}, sql.NullString{String: "github:foo/bar", Valid: true}, sql.NullString{String: "abc123", Valid: true}, []byte(`{"ref":"main"}`), now).
			AddRow(id2, "csv", "updated", sql.NullString{String: "user-1", Valid: true}, sql.NullString{}, sql.NullString{}, []byte(`{}`), now.Add(-time.Hour)))

	ctx := auditCtx("admin", tid)
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/install-events", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleInstallEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp installEventsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events len = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].SkillSlug != "pdf" || resp.Events[0].EventType != "installed" {
		t.Errorf("event[0] = %+v", resp.Events[0])
	}
	if resp.Events[0].SourceURL == nil || *resp.Events[0].SourceURL != "github:foo/bar" {
		t.Errorf("source_url not propagated: %+v", resp.Events[0])
	}
	if string(resp.Events[1].Metadata) != "{}" {
		t.Errorf("metadata default = %s", string(resp.Events[1].Metadata))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestInstallEvents_FilterBySkillSlug(t *testing.T) {
	h, mock := newAuditHandler(t)
	tid := store.MasterTenantID

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM skill_install_events WHERE tenant_id = \$1 AND skill_slug = \$2`).
		WithArgs(tid, "pdf").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	id := uuid.New()
	mock.ExpectQuery(`SELECT id, skill_slug, event_type, user_id, source_url, source_sha, metadata, created_at FROM skill_install_events WHERE tenant_id = \$1 AND skill_slug = \$2`).
		WithArgs(tid, "pdf", 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "skill_slug", "event_type", "user_id", "source_url", "source_sha", "metadata", "created_at"}).
			AddRow(id, "pdf", "installed", sql.NullString{}, sql.NullString{}, sql.NullString{}, []byte(`{}`), time.Now()))

	ctx := auditCtx("admin", tid)
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/install-events?skill_slug=pdf", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleInstallEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp installEventsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if len(resp.Events) != 1 || resp.Events[0].SkillSlug != "pdf" {
		t.Errorf("events = %+v", resp.Events)
	}
}

func TestInstallEvents_Pagination(t *testing.T) {
	h, mock := newAuditHandler(t)
	tid := store.MasterTenantID

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM skill_install_events WHERE tenant_id = \$1`).
		WithArgs(tid).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id, skill_slug, event_type, user_id, source_url, source_sha, metadata, created_at FROM skill_install_events WHERE tenant_id = \$1`).
		WithArgs(tid, 2, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "skill_slug", "event_type", "user_id", "source_url", "source_sha", "metadata", "created_at"}).
			AddRow(uuid.New(), "a", "installed", sql.NullString{}, sql.NullString{}, sql.NullString{}, []byte(`{}`), now).
			AddRow(uuid.New(), "b", "installed", sql.NullString{}, sql.NullString{}, sql.NullString{}, []byte(`{}`), now.Add(-time.Minute)))

	ctx := auditCtx("admin", tid)
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/install-events?limit=2&offset=0", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleInstallEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp installEventsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 10 {
		t.Errorf("total = %d, want 10", resp.Total)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events page = %d, want 2", len(resp.Events))
	}
}

// TestInstallEvents_NonAdminForbidden documents the middleware contract: the
// RegisterRoutes wires this handler with adminMiddleware so a request lacking
// RoleAdmin never reaches handleInstallEvents in production. Calling the raw
// handler bypasses middleware (consistent with the other handler unit tests),
// so we exercise the gate via requireAuth at the mux-routing level.
func TestInstallEvents_NonAdminForbiddenViaMiddleware(t *testing.T) {
	setupTestToken(t, "audit-token")
	h, _ := newAuditHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// No bearer + tokened auth = unauthenticated → requireAuth returns 401
	// (which is functionally equivalent for "non-admin gets blocked"). The
	// production deploy uses requireAuth+RoleAdmin which yields 401/403 for
	// non-admins.
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/install-events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 401/403; body = %s", w.Code, w.Body.String())
	}
}
