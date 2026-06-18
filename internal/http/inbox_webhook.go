package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// Push (Composio email triggers → webhook → WS event) augments the polling
// inbox handler. Composio is configured (dashboard) to POST trigger events to
// /v1/webhooks/composio; we verify the Svix signature, read the owning user
// from the payload, and broadcast a per-user `inbox.updated` event so the
// extension/dashboard refreshes instantly instead of waiting for the next poll.

// EnablePush wires the push half. secret is the Composio webhook signing secret
// (Svix `whsec_…`); pub broadcasts the WS event; tenants resolves user→tenant
// for event scoping. Safe no-op fields when left unset (polling still works).
func (h *InboxHandler) EnablePush(secret string, pub bus.EventPublisher, tenants store.TenantStore) {
	h.webhookSecret = secret
	h.pub = pub
	h.tenants = tenants
}

// pushReady reports whether push is fully wired.
func (h *InboxHandler) pushReady() bool {
	return h.webhookSecret != "" && h.pub != nil && h.tenants != nil
}

// handleComposioWebhook receives Composio trigger deliveries. Always returns
// 200 once authenticated (Composio retries non-2xx); unauthenticated/garbage
// requests get 400 so they're not silently accepted.
func (h *InboxHandler) handleComposioWebhook(w http.ResponseWriter, r *http.Request) {
	if h.webhookSecret == "" {
		http.Error(w, "push not configured", http.StatusServiceUnavailable)
		return
	}
	const maxBody = 4 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}

	// Svix verification: HMAC-SHA256(secret, "<id>.<ts>.<body>") == webhook-signature.
	id := r.Header.Get("webhook-id")
	ts := r.Header.Get("webhook-timestamp")
	sig := r.Header.Get("webhook-signature")
	if !verifyComposioSignature(h.webhookSecret, id, ts, sig, body) {
		slog.Warn("security.composio_webhook_signature_invalid", "remote_addr", r.RemoteAddr, "id", id)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	userID, provider, accountID := parseComposioTriggerEvent(body)
	if userID == "" {
		// Authenticated but unmappable — ack so Composio stops retrying.
		slog.Info("inbox.webhook_unmapped", "id", id, "shape", jsonShape(string(body)))
		w.WriteHeader(http.StatusOK)
		return
	}

	if h.pushReady() {
		tenantID, terr := h.tenants.ResolveUserTenant(r.Context(), userID)
		if terr != nil {
			slog.Info("inbox.webhook_tenant_unresolved", "user", userID, "err", terr.Error())
		} else {
			bus.BroadcastForTenant(h.pub, protocol.EventInboxUpdated, tenantID, map[string]any{
				"user_id":    userID,
				"provider":   provider,
				"account_id": accountID,
			})
			slog.Info("inbox.webhook_pushed", "user", userID, "provider", provider)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// verifyComposioSignature implements Svix-style verification. The signing key is
// the base64 payload after the "whsec_" prefix; the signed content is
// "<id>.<timestamp>.<rawBody>"; webhook-signature is a space-separated list of
// "v1,<base64sig>". A 5-minute timestamp tolerance guards against replay.
func verifyComposioSignature(secret, id, ts, sigHeader string, body []byte) bool {
	if id == "" || ts == "" || sigHeader == "" {
		return false
	}
	if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
		if d := time.Since(time.Unix(n, 0)); d > 5*time.Minute || d < -5*time.Minute {
			return false
		}
	}
	key := strings.TrimPrefix(secret, "whsec_")
	keyBytes, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		keyBytes = []byte(secret) // fall back to raw secret
	}
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, part := range strings.Fields(sigHeader) {
		v := part
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[i+1:] // strip the "v1," version prefix
		}
		if hmac.Equal([]byte(v), []byte(expected)) {
			return true
		}
	}
	return false
}

// parseComposioTriggerEvent leniently pulls the mailbox owner (our userID =
// Composio clientUniqueUserId), provider, and connected-account id out of a
// trigger payload. The exact envelope varies by payload version (V1/V2/V3), so
// we search known field paths defensively rather than bind one schema.
func parseComposioTriggerEvent(body []byte) (userID, provider, accountID string) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", "", ""
	}
	userID = deepFindString(m, "clientUniqueUserId", "client_unique_user_id", "userId", "user_id")
	accountID = deepFindString(m, "connectedAccountNanoId", "connected_account_id", "connectedAccountId", "connection_id")
	slug := deepFindString(m, "triggerName", "trigger_name", "triggerSlug", "trigger_slug", "appName", "app_name")
	switch {
	case strings.Contains(strings.ToUpper(slug), "GMAIL") || strings.EqualFold(slug, "gmail"):
		provider = "gmail"
	case strings.Contains(strings.ToUpper(slug), "OUTLOOK") || strings.EqualFold(slug, "outlook"):
		provider = "outlook"
	}
	return userID, provider, accountID
}

// deepFindString walks a decoded JSON tree and returns the first string value
// found for any of the given keys (case-insensitive), at any depth.
func deepFindString(v any, keys ...string) string {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[strings.ToLower(k)] = true
	}
	var walk func(any) string
	walk = func(node any) string {
		switch t := node.(type) {
		case map[string]any:
			for k, val := range t {
				if want[strings.ToLower(k)] {
					if s, ok := val.(string); ok && s != "" {
						return s
					}
				}
			}
			for _, val := range t {
				if s := walk(val); s != "" {
					return s
				}
			}
		case []any:
			for _, val := range t {
				if s := walk(val); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(v)
}

// ── Trigger provisioning ──────────────────────────────────────────────────

// ProvisionTriggers ensures Composio "new mail" triggers exist for each of the
// user's connected Gmail/Outlook accounts. Idempotent + deduped per process so
// it can be called liberally (e.g. lazily from the unread poll). Best-effort.
func (h *InboxHandler) ProvisionTriggers(ctx context.Context, userID string) {
	if userID == "" || h.webhookSecret == "" {
		return // only bother when push is configured
	}
	for _, toolkit := range []string{"gmail", "outlook"} {
		for _, a := range h.listAccounts(ctx, userID, toolkit) {
			dedupKey := userID + "|" + toolkit + "|" + a.ID
			if _, seen := h.provisioned.LoadOrStore(dedupKey, true); seen {
				continue
			}
			if err := h.subscribeTrigger(ctx, userID, toolkit, a.ID); err != nil {
				h.provisioned.Delete(dedupKey) // allow a retry next poll
				slog.Info("inbox.trigger_subscribe_failed", "user", userID, "toolkit", toolkit, "err", err.Error())
			}
		}
	}
}

// subscribeTrigger asks composio-mcp to enable a trigger for one account.
func (h *InboxHandler) subscribeTrigger(ctx context.Context, userID, toolkit, accountID string) error {
	payload, _ := json.Marshal(map[string]any{"toolkit": toolkit, "connectedAccountId": accountID})
	req, err := http.NewRequestWithContext(ctx, "POST", h.composioURL+"/triggers/subscribe", bytes.NewReader(payload))
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
		return fmt.Errorf("composio-mcp subscribe status %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
