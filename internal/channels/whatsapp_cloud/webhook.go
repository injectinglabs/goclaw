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

// WebhookPath is the single shared route the gateway mounts once at startup;
// Meta delivers every instance's events here, routed by phone_number_id.
const WebhookPath = "/channels/whatsapp_cloud/webhook"

var (
	webhookBase atomic.Pointer[string] // public HTTPS base, no trailing slash
	// registry maps phone_number_id → *Channel for runtime dispatch. One mounted
	// route serves all instances; instances connected after startup are reachable
	// immediately on Start (no re-mount).
	registry sync.Map
)

// ConfigureWebhook installs the public base URL used to build the callback URL
// shown to the user. Called once at startup.
func ConfigureWebhook(baseURL string) {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	webhookBase.Store(&b)
}

// WebhookConfigured reports whether a public base URL is available.
func WebhookConfigured() bool {
	b := webhookBase.Load()
	return b != nil && *b != ""
}

// CallbackURL returns the full webhook URL to paste into Meta's app config.
func CallbackURL() string {
	if b := webhookBase.Load(); b != nil && *b != "" {
		return *b + WebhookPath
	}
	return ""
}

func registerChannel(phoneNumberID string, c *Channel) { registry.Store(phoneNumberID, c) }
func unregisterChannel(phoneNumberID string)           { registry.Delete(phoneNumberID) }

func channelFor(phoneNumberID string) (*Channel, bool) {
	if v, ok := registry.Load(phoneNumberID); ok {
		if c, ok := v.(*Channel); ok {
			return c, true
		}
	}
	return nil, false
}

// verifyTokenMatches reports whether any live instance was configured with this
// verify token. Meta's webhook is configured once per app with one verify
// token, so a match against any registered instance authenticates the GET
// subscription handshake.
func verifyTokenMatches(token string) bool {
	if token == "" {
		return false
	}
	found := false
	registry.Range(func(_, v any) bool {
		if c, ok := v.(*Channel); ok && c.verifyToken != "" && c.verifyToken == token {
			found = true
			return false
		}
		return true
	})
	return found
}

// WebhookDispatcher returns the shared HTTP handler. GET = Meta's subscription
// verification (echo hub.challenge when hub.verify_token matches). POST =
// inbound events: route by phone_number_id, verify the X-Hub-Signature-256 HMAC
// with that instance's app secret, then hand each message to the agent.
func WebhookDispatcher() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			q := r.URL.Query()
			if q.Get("hub.mode") == "subscribe" && verifyTokenMatches(q.Get("hub.verify_token")) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(q.Get("hub.challenge")))
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Always 200 so Meta doesn't retry for 24h on our processing hiccups.
		defer w.WriteHeader(http.StatusOK)

		var payload metaWebhook
		if err := json.Unmarshal(body, &payload); err != nil {
			slog.Warn("whatsapp_cloud: webhook parse failed", "error", err)
			return
		}
		sig := r.Header.Get("X-Hub-Signature-256")
		payload.dispatch(body, sig)
	})
}

// ── Meta webhook payload ──────────────────────────────────────────────────

type metaWebhook struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
					WaID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
				// statuses (delivery receipts) are present but ignored.
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// dispatch routes each inbound text message to its instance after verifying the
// signature with that instance's app secret.
func (p *metaWebhook) dispatch(rawBody []byte, signature string) {
	for _, entry := range p.Entry {
		for _, ch := range entry.Changes {
			pnid := ch.Value.Metadata.PhoneNumberID
			if pnid == "" || len(ch.Value.Messages) == 0 {
				continue // status updates / non-message changes
			}
			c, ok := channelFor(pnid)
			if !ok {
				slog.Warn("whatsapp_cloud: no live instance for phone_number_id", "phone_number_id", pnid)
				continue
			}
			// Verify authenticity before acting on the messages.
			if c.appSecret != "" && !verifySignature(rawBody, signature, c.appSecret) {
				slog.Warn("whatsapp_cloud: signature verification failed", "phone_number_id", pnid)
				continue
			}
			senderName := ""
			if len(ch.Value.Contacts) > 0 {
				senderName = ch.Value.Contacts[0].Profile.Name
			}
			for _, m := range ch.Value.Messages {
				if m.Type != "text" || m.Text.Body == "" {
					continue // v0: text only
				}
				if !c.IsAllowed(m.From) {
					continue
				}
				meta := map[string]string{"wa_message_id": m.ID}
				if senderName != "" {
					meta["sender_name"] = senderName
				}
				// DM-only: sender == chat (the user's phone). HandleMessage
				// derives tenant/agent from the channel itself. Cloud API groups
				// are a separate, limited surface — deferred.
				c.HandleMessage(m.From, m.From, m.Text.Body, nil, meta, "direct")
			}
		}
	}
}

// verifySignature validates Meta's X-Hub-Signature-256 (HMAC-SHA256 of the raw
// body keyed by the app secret). Mirrors the facebook channel's check.
func verifySignature(body []byte, signature, appSecret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	expected, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}
