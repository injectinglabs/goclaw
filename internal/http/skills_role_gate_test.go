package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// ---------------------------------------------------------------------------
// Role-gate tests for skill install / visibility / toggle / delete.
//
// All tests build a SkillsHandler against an in-memory skillManageStoreStub
// and a mockTenantStore. The tenant store is the source of truth for
// IsOwnerOrAdmin — its tenant_user membership count plus per-user role decide
// whether the caller is allowed to perform team-wide writes.
// ---------------------------------------------------------------------------

const roleGateDeniedSubstring = "Only owners and admins can install skills for the team"

// newRoleGateHandler builds a SkillsHandler whose tenantStore is the supplied
// mockTenantStore so IsOwnerOrAdmin is exercised end-to-end.
func newRoleGateHandler(t *testing.T, ts *mockTenantStore) (*SkillsHandler, *skillManageStoreStub, string) {
	t.Helper()
	root := t.TempDir()
	baseDir := filepath.Join(root, "skills-store")
	skillStore := newSkillManageStoreStub(baseDir)
	handler := NewSkillsHandler(skillStore, baseDir, root, "", bus.New(), nil, ts, nil)
	return handler, skillStore, root
}

// roleGateCtx builds a request context with the given user/tenant/locale.
func roleGateCtx(userID string, tenantID uuid.UUID) context.Context {
	return store.WithLocale(
		store.WithTenantID(
			store.WithUserID(context.Background(), userID),
			tenantID,
		),
		"en",
	)
}

// stubInstallDeps stubs out the dependency install/check hooks so the install
// pipeline runs cleanly without touching pip/npm.
func stubInstallDeps(t *testing.T) {
	t.Helper()
	stubUploadDepFns(t,
		func(context.Context, *skills.SkillManifest, []string) (*skills.InstallResult, error) {
			return nil, nil
		},
		func(*skills.SkillManifest) (bool, []string) { return true, nil },
	)
}

// installSkill drives POST /v1/skills/install through the in-process GitHub
// mock. Returns the recorded response.
func installSkill(t *testing.T, h *SkillsHandler, ctx context.Context, slug, visibility string) *httptest.ResponseRecorder {
	t.Helper()
	const (
		owner = "foo"
		repo  = "bar"
		ref   = "main"
	)
	// Vary the SHA per slug so each install hits a fresh mock URL.
	sha := strings.Repeat("a", 40)
	if len(slug) > 0 {
		// Pad the slug to 40 hex chars to form a deterministic distinct SHA.
		filler := strings.Repeat("b", 40-len(slug))
		if len(slug) > 40 {
			sha = slug[:40]
		} else {
			sha = slug + filler
		}
		// Replace any non-hex chars with 'c' so the mock URL is well-formed.
		sb := []byte(sha)
		for i, c := range sb {
			if !isHex(c) {
				sb[i] = 'c'
			}
		}
		sha = string(sb)
	}
	skillMD := "---\nname: " + slug + "\nslug: " + slug + "\n---\nbody\n"
	srv := rawGitHubMock(t, owner, repo, ref, sha, map[string]string{
		"SKILL.md": skillMD,
	})
	defer srv.Close()
	withGitHubBases(t, srv)

	payload := map[string]string{
		"source": "github:" + owner + "/" + repo + "@" + ref,
	}
	if visibility != "" {
		payload["visibility"] = visibility
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleInstall(w, req)
	return w
}

func isHex(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f':
		return true
	}
	return false
}

// ---- POST /v1/skills/install ----

func TestRoleGate_PersonalTenant_MemberCanInstallPublic(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "personal")
	// Personal tenant: exactly one tenant_users row.
	ts.setUserRole(tenantID, "user-solo", store.TenantRoleOwner)

	h, _, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("user-solo", tenantID)

	w := installSkill(t, h, ctx, "personal-public", "public")
	if w.Code != http.StatusCreated {
		t.Fatalf("personal-tenant public install: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestRoleGate_TeamTenant_OwnerCanInstallPublic(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, _, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("owner-user", tenantID)

	w := installSkill(t, h, ctx, "owner-public", "public")
	if w.Code != http.StatusCreated {
		t.Fatalf("team owner public install: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestRoleGate_TeamTenant_AdminCanInstallPublic(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "admin-user", store.TenantRoleAdmin)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, _, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("admin-user", tenantID)

	w := installSkill(t, h, ctx, "admin-public", "public")
	if w.Code != http.StatusCreated {
		t.Fatalf("team admin public install: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestRoleGate_TeamTenant_MemberInstallPublic_Forbidden(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, _, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	w := installSkill(t, h, ctx, "member-public", "public")
	if w.Code != http.StatusForbidden {
		t.Fatalf("team member public install: status = %d, body = %s (want 403)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), roleGateDeniedSubstring) {
		t.Fatalf("body = %s, expected substring %q", w.Body.String(), roleGateDeniedSubstring)
	}
}

func TestRoleGate_TeamTenant_MemberInstallPrivate_Allowed(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, _, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	w := installSkill(t, h, ctx, "member-private", "private")
	if w.Code != http.StatusCreated {
		t.Fatalf("team member private install: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestRoleGate_TeamTenant_MemberInstallDefault_DefaultsToPrivate(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	// Visibility omitted: members should be silently scoped to "private"
	// instead of seeing a 403.
	w := installSkill(t, h, ctx, "member-default", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("team member default install: status = %d, body = %s", w.Code, w.Body.String())
	}
	// Confirm the stored skill is private.
	for _, info := range skillStore.skills {
		if info.Slug == "member-default" && info.Visibility != "private" {
			t.Fatalf("default visibility for member = %q, want private", info.Visibility)
		}
	}
}

// ---- PUT /v1/skills/{id} (handleUpdate) ----

// Verify that a team member who owns a private skill cannot escalate it to
// public — the role-gate on the visibility update fires.
func TestRoleGate_TeamTenant_MemberCannotEscalateOwnSkillToPublic(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	// Seed a private skill owned by "member-user" directly via the store stub.
	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID:         skillID.String(),
		Name:       "Existing Private",
		Slug:       "existing-private",
		Visibility: "private",
		Version:    1,
		Status:     "active",
		Enabled:    true,
	}
	skillStore.ownerByID[skillID] = "member-user"
	skillStore.ownerBySlug["existing-private"] = "member-user"

	body, _ := json.Marshal(map[string]string{"visibility": "public"})
	req := httptest.NewRequest(http.MethodPut, "/v1/skills/"+skillID.String(), bytes.NewReader(body)).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleUpdate(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("escalation by member: status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), roleGateDeniedSubstring) {
		t.Fatalf("body = %s, expected substring %q", w.Body.String(), roleGateDeniedSubstring)
	}
	// Verify visibility wasn't mutated.
	if skillStore.skills[skillID].Visibility != "private" {
		t.Fatalf("visibility leaked to %q despite 403", skillStore.skills[skillID].Visibility)
	}
}

// A tenant admin can flip their own (or shared) skill to public.
func TestRoleGate_TeamTenant_AdminCanEscalateToPublic(t *testing.T) {
	stubInstallDeps(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "admin-user", store.TenantRoleAdmin)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("admin-user", tenantID)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Admin Owned", Slug: "admin-owned",
		Visibility: "private", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "admin-user"
	skillStore.ownerBySlug["admin-owned"] = "admin-user"

	body, _ := json.Marshal(map[string]string{"visibility": "public"})
	req := httptest.NewRequest(http.MethodPut, "/v1/skills/"+skillID.String(), bytes.NewReader(body)).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("admin escalation: status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := skillStore.skills[skillID].Visibility; got != "public" {
		t.Fatalf("visibility = %q, want public", got)
	}
}

// ---- POST /v1/skills/{id}/toggle ----

// Toggling a public skill that a member doesn't own succeeds — but
// scopes the change to the caller via skill_user_disables (cascade-
// by-slug), never touching the canonical row's enabled flag. Response
// must say scope="user" so the SPA can distinguish from an admin-
// owned global flip.
func TestRoleGate_TeamTenant_MemberTogglePublicScopesToUser(t *testing.T) {
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Shared", Slug: "shared",
		Visibility: "public", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "owner-user"
	skillStore.ownerBySlug["shared"] = "owner-user"

	body, _ := json.Marshal(map[string]bool{"enabled": false})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/"+skillID.String()+"/toggle", bytes.NewReader(body)).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleToggle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("member toggle public: status = %d, body = %s; want 200", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"scope":"user"`) {
		t.Fatalf("body = %s, expected scope=user (per-user overlay, not global flip)", w.Body.String())
	}
	// The shared row itself must still be enabled in the store —
	// only the per-user overlay table changed.
	if got := skillStore.skills[skillID]; !got.Enabled {
		t.Fatalf("canonical row enabled was flipped (got %v); cascade should only write skill_user_disables", got.Enabled)
	}
}

// The skill's owner can toggle their own private skill without privileged role.
func TestRoleGate_TeamTenant_OwnerCanTogglePrivateOwnSkill(t *testing.T) {
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Mine", Slug: "mine",
		Visibility: "private", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "member-user"
	skillStore.ownerBySlug["mine"] = "member-user"

	body, _ := json.Marshal(map[string]bool{"enabled": false})
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/"+skillID.String()+"/toggle", bytes.NewReader(body)).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleToggle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("owner toggle own private: status = %d, body = %s", w.Code, w.Body.String())
	}
}

// ---- DELETE /v1/skills/{id} ----

// Force the auth fallback to *not* return RoleAdmin so the ownership branch
// fires the way it would for a paired browser/operator caller.
func enableTokenedAuth(t *testing.T) {
	t.Helper()
	// Setting any non-empty token disables the "no auth → RoleAdmin" fallback
	// in resolveAuth without us having to also supply a matching Authorization
	// header on each request. Callers without a bearer thus look like
	// unauthenticated requests (Role: ""), which exercises the ownership /
	// role-gate code paths in handleDelete / handleUpdate.
	setupTestToken(t, "role-gate-token")
}

func TestRoleGate_TeamTenant_MemberCanDeleteOwnPrivateSkill(t *testing.T) {
	enableTokenedAuth(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Mine", Slug: "mine",
		Visibility: "private", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "member-user"
	skillStore.ownerBySlug["mine"] = "member-user"

	req := httptest.NewRequest(http.MethodDelete, "/v1/skills/"+skillID.String(), nil).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	w := httptest.NewRecorder()
	h.handleDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("member delete own private: status = %d, body = %s", w.Code, w.Body.String())
	}
}

// Per-user model (migration 000071): a member who didn't install a
// skill cannot delete the row, even when it's been shared
// (visibility=public). Their own catalog only ever holds rows they
// created themselves. Public shared row from another user is
// displayed read-only and DELETE returns 403.
func TestRoleGate_TeamTenant_MemberCannotDeleteOthersPublicSkill(t *testing.T) {
	enableTokenedAuth(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("member-user", tenantID)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Shared", Slug: "shared",
		Visibility: "public", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "owner-user"
	skillStore.ownerBySlug["shared"] = "owner-user"

	req := httptest.NewRequest(http.MethodDelete, "/v1/skills/"+skillID.String(), nil).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	w := httptest.NewRecorder()
	h.handleDelete(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("member delete on others' public: status = %d, body = %s; want 403", w.Code, w.Body.String())
	}
	if _, ok := skillStore.skills[skillID]; !ok {
		t.Fatalf("skill row disappeared after forbidden delete; row must stay intact")
	}
}

// Admins ARE NOT special. A tenant admin who didn't install a skill
// still cannot delete it. Sharing transfers nothing — it just adds a
// visibility flag on the owner's row.
func TestRoleGate_TeamTenant_AdminCannotDeleteOthersPublicSkill(t *testing.T) {
	enableTokenedAuth(t)
	ts := newMockTenantStore()
	tenantID := uuid.New()
	ts.addTenant(tenantID, "team")
	ts.setUserRole(tenantID, "owner-user", store.TenantRoleOwner)
	ts.setUserRole(tenantID, "admin-user", store.TenantRoleAdmin)
	ts.setUserRole(tenantID, "member-user", store.TenantRoleMember)

	h, skillStore, _ := newRoleGateHandler(t, ts)
	ctx := roleGateCtx("admin-user", tenantID)

	skillID := uuid.New()
	skillStore.skills[skillID] = store.SkillInfo{
		ID: skillID.String(), Name: "Shared", Slug: "shared",
		Visibility: "public", Version: 1, Status: "active", Enabled: true,
	}
	skillStore.ownerByID[skillID] = "owner-user"
	skillStore.ownerBySlug["shared"] = "owner-user"

	req := httptest.NewRequest(http.MethodDelete, "/v1/skills/"+skillID.String(), nil).WithContext(ctx)
	req.SetPathValue("id", skillID.String())
	w := httptest.NewRecorder()
	h.handleDelete(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("admin delete on others' public: status = %d, body = %s; want 403", w.Code, w.Body.String())
	}
	if _, ok := skillStore.skills[skillID]; !ok {
		t.Fatalf("skill row disappeared after forbidden delete; row must stay intact")
	}
}
