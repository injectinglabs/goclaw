package methods

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// RemindersMethods handles reminders.list / markRead / markAllRead / delete.
// Reminders are the DB-backed inbox for cron-delivered messages on internal
// channels (ws/browser). They're persisted independently of cron_jobs so
// one-shot "at" reminders survive auto-deletion of their parent job.
type RemindersMethods struct {
	store store.ReminderStore
}

func NewRemindersMethods(s store.ReminderStore) *RemindersMethods {
	return &RemindersMethods{store: s}
}

func (m *RemindersMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodRemindersList, m.handleList)
	router.Register(protocol.MethodRemindersMarkRead, m.handleMarkRead)
	router.Register(protocol.MethodRemindersMarkAllRead, m.handleMarkAllRead)
	router.Register(protocol.MethodRemindersDelete, m.handleDelete)
}

func (m *RemindersMethods) handleList(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}
	reminders, err := m.store.List(ctx, store.ReminderListOpts{
		UserID: client.UserID(),
		Limit:  params.Limit,
		Offset: params.Offset,
	})
	if err != nil {
		slog.Warn("reminders.list failed", "error", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}
	// Return a non-nil slice so clients that JSON-decode can't end up with null.
	if reminders == nil {
		reminders = []store.Reminder{}
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"reminders": reminders,
		"count":     len(reminders),
	}))
}

func (m *RemindersMethods) handleMarkRead(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params struct {
		ID string `json:"id"`
	}
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}
	id, err := uuid.Parse(params.ID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid id"))
		return
	}
	if err := m.store.MarkRead(ctx, id); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{"ok": true}))
}

func (m *RemindersMethods) handleMarkAllRead(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if err := m.store.MarkAllRead(ctx, client.UserID()); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{"ok": true}))
}

func (m *RemindersMethods) handleDelete(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params struct {
		ID string `json:"id"`
	}
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}
	id, err := uuid.Parse(params.ID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid id"))
		return
	}
	if err := m.store.Delete(ctx, id); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{"ok": true}))
}
