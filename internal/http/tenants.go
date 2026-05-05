package http

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tenantname"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// TenantsHandler handles tenant CRUD and membership endpoints.
type TenantsHandler struct {
	tenantStore store.TenantStore
	msgBus      *bus.MessageBus
	workspace   string // base workspace directory for tenant dirs
}

// NewTenantsHandler creates a handler for tenant management endpoints.
func NewTenantsHandler(tenantStore store.TenantStore, msgBus *bus.MessageBus, workspace string) *TenantsHandler {
	return &TenantsHandler{tenantStore: tenantStore, msgBus: msgBus, workspace: workspace}
}

// RegisterRoutes registers all tenant management routes on the given mux.
func (h *TenantsHandler) RegisterRoutes(mux *http.ServeMux) {
	admin := func(next http.HandlerFunc) http.HandlerFunc {
		return requireAuth(permissions.RoleAdmin, next)
	}
	mux.HandleFunc("GET /v1/tenants", admin(h.handleList))
	mux.HandleFunc("POST /v1/tenants", admin(h.handleCreate))
	mux.HandleFunc("GET /v1/tenants/suggest-name", admin(h.handleSuggestName))
	mux.HandleFunc("GET /v1/tenants/{id}", admin(h.handleGet))
	mux.HandleFunc("PATCH /v1/tenants/{id}", admin(h.handleUpdate))
	mux.HandleFunc("GET /v1/tenants/{id}/users", admin(h.handleUsersList))
	mux.HandleFunc("POST /v1/tenants/{id}/users", admin(h.handleUsersAdd))
	mux.HandleFunc("DELETE /v1/tenants/{id}/users/{userId}", admin(h.handleUsersRemove))
}

func (h *TenantsHandler) handleList(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.list")})
		return
	}

	tenants, err := h.tenantStore.ListTenants(r.Context())
	if err != nil {
		slog.Error("tenants.list failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToList, "tenants")})
		return
	}
	if tenants == nil {
		tenants = []store.TenantData{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants})
}

func (h *TenantsHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.create")})
		return
	}

	var input struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !bindJSON(w, r, locale, &input) {
		return
	}

	// When the caller omits both name and slug we synthesise a friendly,
	// dictionary-based identifier (e.g. "happy-amber-otter"). This keeps
	// auto-provisioned tenants user-presentable without forcing the caller
	// to invent one.
	if input.Slug == "" {
		generated, gerr := tenantname.GenerateUnique(r.Context(), h.slugTaken, 8)
		if gerr != nil {
			slog.Error("tenants.create: name generation failed", "error", gerr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToGenerateName)})
			return
		}
		input.Slug = generated
	}
	if input.Name == "" {
		input.Name = tenantname.DisplayName(input.Slug)
	}
	if !isValidSlug(input.Slug) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidSlug, "slug")})
		return
	}

	tenant := &store.TenantData{
		ID:     store.GenNewID(),
		Name:   input.Name,
		Slug:   input.Slug,
		Status: store.TenantStatusActive,
	}

	if err := h.tenantStore.CreateTenant(r.Context(), tenant); err != nil {
		slog.Error("tenants.create failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToCreate, "tenant", err.Error())})
		return
	}

	// Create workspace directory for the tenant.
	if h.workspace != "" {
		tenantDir := filepath.Join(h.workspace, "tenants", tenant.Slug)
		if err := os.MkdirAll(tenantDir, 0755); err != nil {
			slog.Warn("tenants.create: failed to create workspace dir", "dir", tenantDir, "error", err)
		}
	}

	h.emitCacheInvalidate(bus.CacheKindTenantUsers, tenant.ID.String())
	emitAudit(h.msgBus, r, "tenant.created", "tenant", tenant.ID.String())
	writeJSON(w, http.StatusCreated, tenant)
}

func (h *TenantsHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.get")})
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "tenant")})
		return
	}

	tenant, err := h.tenantStore.GetTenant(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgNotFound, "tenant", id.String())})
		return
	}
	writeJSON(w, http.StatusOK, tenant)
}

func (h *TenantsHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.update")})
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "tenant")})
		return
	}

	var input struct {
		Name     string         `json:"name"`
		Status   string         `json:"status"`
		Settings map[string]any `json:"settings"`
	}
	if !bindJSON(w, r, locale, &input) {
		return
	}

	updates := make(map[string]any)
	if input.Name != "" {
		updates["name"] = input.Name
	}
	if input.Status != "" {
		updates["status"] = input.Status
	}
	if input.Settings != nil {
		updates["settings"] = input.Settings
	}

	if len(updates) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidUpdates)})
		return
	}

	if err := h.tenantStore.UpdateTenant(r.Context(), id, updates); err != nil {
		slog.Error("tenants.update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToUpdate, "tenant", err.Error())})
		return
	}

	h.emitCacheInvalidate(bus.CacheKindTenantUsers, id.String())
	emitAudit(h.msgBus, r, "tenant.updated", "tenant", id.String())
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (h *TenantsHandler) handleUsersList(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.users.list")})
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "tenant")})
		return
	}

	users, err := h.tenantStore.ListUsers(r.Context(), id)
	if err != nil {
		slog.Error("tenants.users.list failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToList, "tenant users")})
		return
	}
	if users == nil {
		users = []store.TenantUserData{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (h *TenantsHandler) handleUsersAdd(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.users.add")})
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "tenant")})
		return
	}

	var input struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if !bindJSON(w, r, locale, &input) {
		return
	}

	if input.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgRequired, "user_id")})
		return
	}
	if input.Role == "" {
		input.Role = store.TenantRoleMember
	}
	validRoles := map[string]bool{
		store.TenantRoleOwner: true, store.TenantRoleAdmin: true,
		store.TenantRoleOperator: true, store.TenantRoleMember: true, store.TenantRoleViewer: true,
	}
	if !validRoles[input.Role] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRole)})
		return
	}

	if err := h.tenantStore.AddUser(r.Context(), id, input.UserID, input.Role); err != nil {
		slog.Error("tenants.users.add failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToCreate, "tenant user", err.Error())})
		return
	}

	h.emitCacheInvalidate(bus.CacheKindTenantUsers, input.UserID)
	emitAudit(h.msgBus, r, "tenant.user.added", "tenant", id.String())
	writeJSON(w, http.StatusCreated, map[string]string{"ok": "true"})
}

func (h *TenantsHandler) handleUsersRemove(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.users.remove")})
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "tenant")})
		return
	}

	userID := r.PathValue("userId")
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgRequired, "userId")})
		return
	}

	if err := h.tenantStore.RemoveUser(r.Context(), id, userID); err != nil {
		slog.Error("tenants.users.remove failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToDelete, "tenant user", err.Error())})
		return
	}

	h.emitCacheInvalidate(bus.CacheKindTenantUsers, userID)
	emitAudit(h.msgBus, r, "tenant.user.removed", "tenant", id.String())

	// Notify affected user's WS sessions to force logout
	if h.msgBus != nil {
		h.msgBus.Broadcast(bus.Event{
			Name:    protocol.EventTenantAccessRevoked,
			Payload: map[string]string{"user_id": userID, "tenant_id": id.String()},
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleSuggestName returns a freshly generated, currently-unused tenant
// name. The frontend calls it before showing the create dialog so the
// "name" field can be pre-filled with a friendly default the user can
// accept or regenerate.
func (h *TenantsHandler) handleSuggestName(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !store.IsOwnerRole(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgPermissionDenied, "tenants.suggest_name")})
		return
	}

	slug, err := tenantname.GenerateUnique(r.Context(), h.slugTaken, 8)
	if err != nil {
		slog.Error("tenants.suggest_name failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToGenerateName)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name": tenantname.DisplayName(slug),
		"slug": slug,
	})
}

// slugTaken adapts TenantStore.GetTenantBySlug to tenantname.SlugTakenFunc.
// sql.ErrNoRows means the slug is free; any other error is propagated so the
// caller does not silently reuse a slug after a transient lookup failure.
func (h *TenantsHandler) slugTaken(ctx context.Context, slug string) (bool, error) {
	_, err := h.tenantStore.GetTenantBySlug(ctx, slug)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func (h *TenantsHandler) emitCacheInvalidate(kind, key string) {
	if h.msgBus == nil {
		return
	}
	h.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: kind, Key: key},
	})
}
