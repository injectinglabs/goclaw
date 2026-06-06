package whatsappcloud

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
)

// WebhookPath is the single app-level route Meta posts all inbound events to.
// (Cloud API uses one callback URL per Meta app; routing to the right instance
// happens by phone_number_id inside the payload — see WebhookDispatcher.)
const WebhookPath = "/whatsapp/webhook"

// settings holds the process-wide, app-level Cloud API config, set once at startup.
type settings struct {
	appSecret    string // for X-Hub-Signature-256 validation
	verifyToken  string // for the GET subscription handshake
	graphVersion string // e.g. "v21.0"
}

var (
	cfg             atomic.Pointer[settings]
	webhookChannels sync.Map // phone_number_id → *Channel
)

// ConfigureWebhook installs the app-level Cloud API settings. graphVersion
// defaults to v21.0 when empty.
func ConfigureWebhook(appSecret, verifyToken, graphVersion string) {
	if graphVersion == "" {
		graphVersion = "v21.0"
	}
	cfg.Store(&settings{appSecret: appSecret, verifyToken: verifyToken, graphVersion: graphVersion})
}

// WebhookConfigured reports whether the app-level secret + verify token are set
// (both required to mount the route).
func WebhookConfigured() bool {
	s := cfg.Load()
	return s != nil && s.appSecret != "" && s.verifyToken != ""
}

// graphBaseURL returns the Graph API base, e.g. https://graph.facebook.com/v21.0
func graphBaseURL() string {
	v := "v21.0"
	if s := cfg.Load(); s != nil && s.graphVersion != "" {
		v = s.graphVersion
	}
	return "https://graph.facebook.com/" + v
}

func registerWebhookChannel(phoneNumberID string, c *Channel) { webhookChannels.Store(phoneNumberID, c) }
func unregisterWebhookChannel(phoneNumberID string)           { webhookChannels.Delete(phoneNumberID) }

// WebhookDispatcher returns the handler for the shared Cloud API route.
//   - GET  → subscription handshake: validate hub.verify_token, echo hub.challenge.
//   - POST → validate X-Hub-Signature-256 over the raw body, then route each
//     message to the instance registered for its phone_number_id.
//
// Mount once at startup: mux.Handle(whatsappcloud.WebhookPath, whatsappcloud.WebhookDispatcher())
func WebhookDispatcher() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleVerify(w, r)
		case http.MethodPost:
			handleEvent(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// handleVerify answers Meta's subscription challenge.
func handleVerify(w http.ResponseWriter, r *http.Request) {
	s := cfg.Load()
	q := r.URL.Query()
	if s != nil && q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == s.verifyToken {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(q.Get("hub.challenge")))
		return
	}
	slog.Warn("security.whatsapp_cloud_verify_failed", "mode", q.Get("hub.mode"))
	http.Error(w, "forbidden", http.StatusForbidden)
}

// handleEvent validates the signature and dispatches inbound messages.
func handleEvent(w http.ResponseWriter, r *http.Request) {
	s := cfg.Load()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if s == nil || !validSignature(s.appSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("security.whatsapp_cloud_bad_signature")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Warn("whatsapp cloud: decode payload failed", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Ack immediately, dispatch in the request goroutine (handlers publish to
	// the bus and return fast); Meta retries on non-2xx.
	dispatchPayload(payload)
	w.WriteHeader(http.StatusOK)
}

// validSignature verifies X-Hub-Signature-256: sha256=<hex> = HMAC-SHA256(appSecret, rawBody).
// Computed over the raw body (Meta escapes unicode), compared in constant time.
func validSignature(appSecret string, body []byte, header string) bool {
	if appSecret == "" {
		return false
	}
	want := strings.TrimPrefix(header, "sha256=")
	if want == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}
