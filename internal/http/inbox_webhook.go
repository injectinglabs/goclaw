package http

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// Push (Composio email triggers → composio-mcp socket → internal forward → WS)
// augments the polling inbox handler. composio-mcp holds a long-lived
// `composio.triggers.subscribe()` connection (it owns the SDK), and forwards
// each new-mail event to this service via POST /v1/internal/inbox-event. We
// then broadcast a per-user `inbox.updated` event so the extension/dashboard
// refreshes instantly instead of waiting for the next poll. Triggers must first
// be created per account (ProvisionTriggers → composio-mcp /triggers/enable).

// EnablePush wires the push half. pub broadcasts the WS event; tenants resolves
// user→tenant for event scoping; internalToken (optional) authenticates the
// composio-mcp → goclaw forward. With push enabled, ProvisionTriggers runs and
// the internal endpoint is live; otherwise the handler stays polling-only.
func (h *InboxHandler) EnablePush(pub bus.EventPublisher, tenants store.TenantStore, internalToken string) {
	h.pub = pub
	h.tenants = tenants
	h.internalToken = internalToken
	h.pushEnabled = pub != nil && tenants != nil
}

// handleInternalInboxEvent receives a forwarded trigger event from composio-mcp
// (same private network). Body: {user_id, provider, account_id}. Optional shared
// token guards it; the route is not publicly auth'd (composio-mcp has no user
// JWT), so it relies on network isolation + the token for defense in depth.
func (h *InboxHandler) handleInternalInboxEvent(w http.ResponseWriter, r *http.Request) {
	if !h.pushEnabled {
		http.Error(w, "push not enabled", http.StatusServiceUnavailable)
		return
	}
	if h.internalToken != "" {
		got := r.Header.Get("X-Inbox-Push-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.internalToken)) != 1 {
			slog.Warn("security.inbox_internal_token_invalid", "remote_addr", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var ev struct {
		UserID    string `json:"user_id"`
		Provider  string `json:"provider"`
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(body, &ev) != nil || ev.UserID == "" {
		http.Error(w, "user_id required", http.StatusBadRequest)
		return
	}
	h.pushInboxUpdated(r.Context(), ev.UserID, ev.Provider, ev.AccountID)
	w.WriteHeader(http.StatusOK)
}

// pushInboxUpdated resolves the user's tenant and broadcasts a per-user
// inbox.updated event (scoped fail-closed in event_filter.go).
func (h *InboxHandler) pushInboxUpdated(ctx context.Context, userID, provider, accountID string) {
	if !h.pushEnabled || userID == "" {
		return
	}
	tenantID, err := h.tenants.ResolveUserTenant(ctx, userID)
	if err != nil {
		slog.Info("inbox.push_tenant_unresolved", "user", userID, "err", err.Error())
		return
	}
	bus.BroadcastForTenant(h.pub, protocol.EventInboxUpdated, tenantID, map[string]any{
		"user_id":    userID,
		"provider":   provider,
		"account_id": accountID,
	})
	slog.Info("inbox.pushed", "user", userID, "provider", provider)

	// Best-effort Web Push to the user's subscribed browsers. Scope the ctx to
	// the resolved tenant so VAPID key lookup resolves correctly. Never blocks.
	if h.pushSender != nil {
		payload, err := json.Marshal(map[string]string{
			"title":    "New email",
			"body":     provider,
			"provider": provider,
		})
		if err != nil {
			slog.Info("inbox.push_payload_marshal_failed", "user", userID, "err", err.Error())
			return
		}
		h.pushSender(store.WithTenantID(ctx, tenantID), userID, payload)
	}
}

// ── Trigger provisioning ──────────────────────────────────────────────────

// ProvisionTriggers ensures Composio "new mail" triggers exist for each of the
// user's connected Gmail/Outlook accounts so composio-mcp's subscription
// receives their events. Idempotent + deduped per process; best-effort.
func (h *InboxHandler) ProvisionTriggers(ctx context.Context, userID string) {
	if userID == "" || !h.pushEnabled {
		return
	}
	for _, toolkit := range []string{"gmail", "outlook"} {
		for _, a := range h.listAccounts(ctx, userID, toolkit) {
			dedupKey := userID + "|" + toolkit + "|" + a.ID
			if _, seen := h.provisioned.LoadOrStore(dedupKey, true); seen {
				continue
			}
			if err := h.enableTrigger(ctx, userID, toolkit, a.ID); err != nil {
				h.provisioned.Delete(dedupKey) // allow a retry next poll
				slog.Info("inbox.trigger_enable_failed", "user", userID, "toolkit", toolkit, "err", err.Error())
			}
		}
	}
}

// enableTrigger asks composio-mcp to create a trigger for one account.
func (h *InboxHandler) enableTrigger(ctx context.Context, userID, toolkit, accountID string) error {
	payload, _ := json.Marshal(map[string]any{"toolkit": toolkit, "connectedAccountId": accountID})
	req, err := http.NewRequestWithContext(ctx, "POST", h.composioURL+"/triggers/enable", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Proxy-User", userID)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("composio-mcp enable status %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
