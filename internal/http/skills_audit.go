package http

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// installEvent is one row in the GET /v1/skills/install-events response.
type installEvent struct {
	ID        string          `json:"id"`
	SkillSlug string          `json:"skill_slug"`
	EventType string          `json:"event_type"`
	UserID    *string         `json:"user_id,omitempty"`
	SourceURL *string         `json:"source_url,omitempty"`
	SourceSHA *string         `json:"source_sha,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

// installEventsResponse is the body returned by GET /v1/skills/install-events.
type installEventsResponse struct {
	Events []installEvent `json:"events"`
	Total  int            `json:"total"`
}

// handleInstallEvents returns paginated skill install / update audit events
// for the active tenant. Admin-only.
func (h *SkillsHandler) handleInstallEvents(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "db not configured"})
		return
	}

	ctx := r.Context()
	tid := store.TenantIDFromContext(ctx)
	if tid == uuid.Nil {
		tid = store.MasterTenantID
	}

	limit := 50
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	skillSlug := strings.TrimSpace(r.URL.Query().Get("skill_slug"))
	eventType := strings.TrimSpace(r.URL.Query().Get("event_type"))

	// Build the WHERE clause dynamically with positional args.
	args := []any{tid}
	where := "tenant_id = $1"
	if skillSlug != "" {
		args = append(args, skillSlug)
		where += " AND skill_slug = $" + strconv.Itoa(len(args))
	}
	if eventType != "" {
		args = append(args, eventType)
		where += " AND event_type = $" + strconv.Itoa(len(args))
	}

	// Total count first (cheap with the existing tenant index).
	var total int
	if err := h.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM skill_install_events WHERE "+where, args...,
	).Scan(&total); err != nil {
		slog.Warn("skills.install_events: count failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}

	args = append(args, limit, offset)
	rows, err := h.db.QueryContext(ctx,
		"SELECT id, skill_slug, event_type, user_id, source_url, source_sha, metadata, created_at "+
			"FROM skill_install_events WHERE "+where+
			" ORDER BY created_at DESC LIMIT $"+strconv.Itoa(len(args)-1)+
			" OFFSET $"+strconv.Itoa(len(args)),
		args...,
	)
	if err != nil {
		slog.Warn("skills.install_events: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	resp := installEventsResponse{Total: total, Events: []installEvent{}}
	for rows.Next() {
		var (
			id        uuid.UUID
			slug      string
			etype     string
			userID    sql.NullString
			sourceURL sql.NullString
			sourceSHA sql.NullString
			meta      []byte
			createdAt time.Time
		)
		if err := rows.Scan(&id, &slug, &etype, &userID, &sourceURL, &sourceSHA, &meta, &createdAt); err != nil {
			slog.Warn("skills.install_events: scan failed", "error", err)
			continue
		}
		evt := installEvent{
			ID:        id.String(),
			SkillSlug: slug,
			EventType: etype,
			CreatedAt: createdAt.UTC(),
		}
		if userID.Valid {
			u := userID.String
			evt.UserID = &u
		}
		if sourceURL.Valid {
			s := sourceURL.String
			evt.SourceURL = &s
		}
		if sourceSHA.Valid {
			s := sourceSHA.String
			evt.SourceSHA = &s
		}
		if len(meta) > 0 {
			evt.Metadata = json.RawMessage(meta)
		} else {
			evt.Metadata = json.RawMessage("{}")
		}
		resp.Events = append(resp.Events, evt)
	}

	writeJSON(w, http.StatusOK, resp)
}
