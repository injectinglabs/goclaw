package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// System config keys for the persisted VAPID keypair.
const (
	vapidPublicKeyConfig  = "vapid_public_key"
	vapidPrivateKeyConfig = "vapid_private_key"
)

// PushHandler exposes Web Push registration endpoints (subscribe/unsubscribe +
// VAPID public key) and sends Web Push notifications to a user's subscribed
// browsers (see SendToUser, called from the inbox new-mail path).
type PushHandler struct {
	sysConfigs store.SystemConfigStore
	subs       store.PushSubscriptionStore

	mu         sync.Mutex // guards the lazy generate-and-persist of VAPID keys
	cachedPub  string
	cachedPriv string
}

// NewPushHandler constructs a PushHandler. Both stores are required.
func NewPushHandler(sysConfigs store.SystemConfigStore, subs store.PushSubscriptionStore) *PushHandler {
	return &PushHandler{sysConfigs: sysConfigs, subs: subs}
}

// RegisterRoutes registers the push routes on the given mux.
func (h *PushHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/push/vapid-public-key", requireAuth("", h.handleVapidPublicKey))
	mux.HandleFunc("POST /v1/push/subscribe", requireAuth("", h.handleSubscribe))
	mux.HandleFunc("POST /v1/push/unsubscribe", requireAuth("", h.handleUnsubscribe))
}

// vapidKeys returns the VAPID (public, private) keypair. Resolution order:
//  1. VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY env vars (if both set).
//  2. vapid_public_key / vapid_private_key in system_configs (tenant from ctx).
//  3. Freshly generated keys, persisted to system_configs.
func (h *PushHandler) vapidKeys(ctx context.Context) (pub, priv string, err error) {
	if p, k := os.Getenv("VAPID_PUBLIC_KEY"), os.Getenv("VAPID_PRIVATE_KEY"); p != "" && k != "" {
		return p, k, nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cachedPub != "" && h.cachedPriv != "" {
		return h.cachedPub, h.cachedPriv, nil
	}

	pub, _ = h.sysConfigs.Get(ctx, vapidPublicKeyConfig)
	priv, _ = h.sysConfigs.Get(ctx, vapidPrivateKeyConfig)
	if pub != "" && priv != "" {
		h.cachedPub, h.cachedPriv = pub, priv
		return pub, priv, nil
	}

	// Generate + persist. webpush.GenerateVAPIDKeys returns (private, public, err).
	genPriv, genPub, gErr := webpush.GenerateVAPIDKeys()
	if gErr != nil {
		return "", "", gErr
	}
	if err := h.sysConfigs.Set(ctx, vapidPublicKeyConfig, genPub); err != nil {
		slog.Warn("push.vapid_persist_public_failed", "err", err.Error())
	}
	if err := h.sysConfigs.Set(ctx, vapidPrivateKeyConfig, genPriv); err != nil {
		slog.Warn("push.vapid_persist_private_failed", "err", err.Error())
	}
	h.cachedPub, h.cachedPriv = genPub, genPriv
	return genPub, genPriv, nil
}

// handleVapidPublicKey returns {"publicKey": "<vapid public key>"}.
func (h *PushHandler) handleVapidPublicKey(w http.ResponseWriter, r *http.Request) {
	pub, _, err := h.vapidKeys(r.Context())
	if err != nil {
		slog.Warn("push.vapid_keys_failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "vapid key unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": pub})
}

// handleSubscribe stores a browser push subscription for the authenticated user.
// Body: {"endpoint":"...","keys":{"p256dh":"...","auth":"..."}}.
func (h *PushHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint and keys required"})
		return
	}

	sub := &store.PushSubscription{
		TenantID: store.TenantIDFromContext(r.Context()),
		UserID:   userID,
		Endpoint: body.Endpoint,
		P256dh:   body.Keys.P256dh,
		Auth:     body.Keys.Auth,
	}
	if err := h.subs.Upsert(r.Context(), sub); err != nil {
		slog.Warn("push.subscribe_failed", "user", userID, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "subscribe failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUnsubscribe removes a subscription by endpoint. Body: {"endpoint":"..."}.
func (h *PushHandler) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint required"})
		return
	}
	if err := h.subs.DeleteByEndpoint(r.Context(), body.Endpoint); err != nil {
		slog.Warn("push.unsubscribe_failed", "user", userID, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unsubscribe failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// SendToUser delivers payloadJSON as a Web Push notification to every browser
// the user has subscribed. Best-effort: per-subscription errors are logged and
// skipped; expired endpoints (404/410) are pruned. ctx should carry the user's
// tenant so the VAPID key lookup resolves to the right tenant.
func (h *PushHandler) SendToUser(ctx context.Context, userID string, payloadJSON []byte) {
	if userID == "" || len(payloadJSON) == 0 {
		return
	}
	subs, err := h.subs.ListByUser(ctx, userID)
	if err != nil {
		slog.Warn("push.list_subs_failed", "user", userID, "err", err.Error())
		return
	}
	if len(subs) == 0 {
		return
	}
	pub, priv, err := h.vapidKeys(ctx)
	if err != nil {
		slog.Warn("push.send_vapid_failed", "user", userID, "err", err.Error())
		return
	}

	opts := &webpush.Options{
		Subscriber:      "mailto:noreply@injecting.ai",
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             60,
	}
	for _, s := range subs {
		ws := &webpush.Subscription{
			Endpoint: s.Endpoint,
			Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
		}
		resp, sendErr := webpush.SendNotification(payloadJSON, ws, opts)
		if sendErr != nil {
			slog.Info("push.send_failed", "user", userID, "err", sendErr.Error())
			continue
		}
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusNotFound || status == http.StatusGone {
			if delErr := h.subs.DeleteByEndpoint(ctx, s.Endpoint); delErr != nil {
				slog.Info("push.prune_failed", "user", userID, "err", delErr.Error())
			}
		}
	}
}
