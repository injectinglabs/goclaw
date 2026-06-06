package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mymmrac/telego"
)

// WebhookPathPrefix is the stable route the gateway mounts once at startup.
// Incoming Telegram updates arrive at WebhookPathPrefix + "{instanceID}".
const WebhookPathPrefix = "/telegram/webhook/"

// webhookSettings holds process-wide webhook configuration, set once at startup.
type webhookSettings struct {
	baseURL    string // public HTTPS base, e.g. https://aos-stg.injecting.ai (no trailing slash)
	signingKey string // server secret used to derive per-instance webhook secrets
}

var (
	webhookCfg atomic.Pointer[webhookSettings]
	// webhookChannels maps instanceID → *Channel for runtime dispatch. A single
	// mounted route serves all bots, so instances connected after startup are
	// reachable immediately on registration — no re-mount needed.
	webhookChannels sync.Map
)

// ConfigureWebhook installs the process-wide webhook base URL and signing key.
// Called once during gateway startup. Empty baseURL leaves webhook mode
// unavailable (channels fall back to polling).
func ConfigureWebhook(baseURL, signingKey string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	webhookCfg.Store(&webhookSettings{baseURL: baseURL, signingKey: signingKey})
}

// WebhookConfigured reports whether a public base URL is available for webhook mode.
func WebhookConfigured() bool {
	s := webhookCfg.Load()
	return s != nil && s.baseURL != ""
}

// webhookBaseURL returns the configured public base (no trailing slash), or "".
func webhookBaseURL() string {
	if s := webhookCfg.Load(); s != nil {
		return s.baseURL
	}
	return ""
}

// deriveWebhookSecret produces a stable per-instance secret token from the
// server signing key. Telegram echoes it in the X-Telegram-Bot-Api-Secret-Token
// header on every webhook request; we validate it to authenticate the caller.
// Deterministic derivation avoids persisting yet another secret — it is
// recomputed on each Start (and re-applied via SetWebhook).
func deriveWebhookSecret(instanceID string) string {
	key := ""
	if s := webhookCfg.Load(); s != nil {
		key = s.signingKey
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("telegram-webhook:" + instanceID))
	return hex.EncodeToString(mac.Sum(nil)) // 64 hex chars; within Telegram's allowed charset/length
}

func registerWebhookChannel(instanceID string, c *Channel) { webhookChannels.Store(instanceID, c) }
func unregisterWebhookChannel(instanceID string)           { webhookChannels.Delete(instanceID) }

// WebhookDispatcher returns the HTTP handler for the shared Telegram webhook
// route. It parses the instance ID from the path, looks up the live channel,
// validates Telegram's secret-token header, decodes the update, and feeds it
// into the same dispatch path the long-poll loop uses. Mount once at startup:
//
//	mux.Handle(telegram.WebhookPathPrefix, telegram.WebhookDispatcher())
func WebhookDispatcher() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		instanceID := strings.Trim(strings.TrimPrefix(r.URL.Path, WebhookPathPrefix), "/")
		if instanceID == "" || strings.Contains(instanceID, "/") {
			http.NotFound(w, r)
			return
		}
		v, ok := webhookChannels.Load(instanceID)
		if !ok {
			// Unknown/disconnected instance. 200 so Telegram stops retrying a
			// stale URL; nothing to process.
			slog.Debug("telegram webhook: no channel for instance", "instance", instanceID)
			w.WriteHeader(http.StatusOK)
			return
		}
		c := v.(*Channel)

		// Authenticate: constant-time compare against the per-instance secret.
		got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if c.webhookSecret == "" || !hmac.Equal([]byte(got), []byte(c.webhookSecret)) {
			slog.Warn("security.telegram_webhook_bad_secret", "instance", instanceID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var update telego.Update
		if err := json.Unmarshal(body, &update); err != nil {
			slog.Warn("telegram webhook: decode update failed", "instance", instanceID, "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Dispatch asynchronously (spawns a bounded handler goroutine) and ack
		// immediately — Telegram expects a prompt 2xx or it retries.
		c.dispatchUpdate(c.webhookDispatchCtx(), update)
		w.WriteHeader(http.StatusOK)
	}
}
