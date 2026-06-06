// Package whatsappcloud implements the official WhatsApp Business Cloud API
// channel (Meta Graph API). Unlike the whatsmeow channel, it is webhook-based
// and stateless: inbound updates arrive at a single app-level webhook route and
// are routed to the right instance by phone_number_id; outbound messages are
// plain HTTPS POSTs to the Graph API. No persistent connection — so it scales
// across replicas like the Telegram webhook channel.
package whatsappcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// maxWhatsAppTextLen is WhatsApp's per-text-message body limit.
const maxWhatsAppTextLen = 4096

// Channel is a WhatsApp Cloud API channel instance (one WABA phone number).
type Channel struct {
	*channels.BaseChannel
	httpClient    *http.Client
	accessToken   string // per-instance Graph API token (send)
	phoneNumberID string // per-instance routing key (inbound) + send path segment
	instanceID    uuid.UUID
}

// New builds a Cloud API channel from decoded credentials.
func New(msgBus *bus.MessageBus, accessToken, phoneNumberID string, allowFrom []string) *Channel {
	base := channels.NewBaseChannel(channels.TypeWhatsAppCloud, msgBus, allowFrom)
	base.SetType(channels.TypeWhatsAppCloud)
	return &Channel{
		BaseChannel:   base,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		accessToken:   accessToken,
		phoneNumberID: phoneNumberID,
	}
}

// SetInstanceID records the DB instance id (for logs/health).
func (c *Channel) SetInstanceID(id uuid.UUID) { c.instanceID = id }

// Start registers this instance in the webhook dispatch registry (keyed by
// phone_number_id) and marks it healthy. There is no connection to open — the
// webhook URL is configured once at the Meta app level — so any replica that
// has loaded the instance can serve its inbound webhooks.
func (c *Channel) Start(_ context.Context) error {
	if c.phoneNumberID == "" {
		return fmt.Errorf("whatsapp_cloud: phone_number_id is required")
	}
	if c.accessToken == "" {
		return fmt.Errorf("whatsapp_cloud: access_token is required")
	}
	registerWebhookChannel(c.phoneNumberID, c)
	c.SetRunning(true)
	c.MarkHealthy(fmt.Sprintf("Connected (phone_number_id %s)", c.phoneNumberID))
	slog.Info("whatsapp cloud channel started", "name", c.Name(), "phone_number_id", c.phoneNumberID)
	return nil
}

// Stop deregisters the instance so inbound webhooks for this number are no
// longer routed here. No connection to tear down.
func (c *Channel) Stop(_ context.Context) error {
	unregisterWebhookChannel(c.phoneNumberID)
	c.SetRunning(false)
	c.MarkStopped("Stopped")
	slog.Info("whatsapp cloud channel stopped", "name", c.Name(), "phone_number_id", c.phoneNumberID)
	return nil
}

// sendTextRequest is the Graph API send-message body for a text message.
type sendTextRequest struct {
	MessagingProduct string `json:"messaging_product"`
	RecipientType    string `json:"recipient_type"`
	To               string `json:"to"`
	Type             string `json:"type"`
	Text             struct {
		PreviewURL bool   `json:"preview_url"`
		Body       string `json:"body"`
	} `json:"text"`
}

// Send delivers an outbound message via the Graph API. Long bodies are split
// into <=4096-char chunks (WhatsApp's text limit), sent in order.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("whatsapp cloud channel not running")
	}
	if msg.Content == "" {
		return nil
	}
	for _, chunk := range chunkText(msg.Content, maxWhatsAppTextLen) {
		if err := c.sendText(ctx, msg.ChatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) sendText(ctx context.Context, to, body string) error {
	var req sendTextRequest
	req.MessagingProduct = "whatsapp"
	req.RecipientType = "individual"
	req.To = to
	req.Type = "text"
	req.Text.PreviewURL = false
	req.Text.Body = body

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal send: %w", err)
	}
	url := fmt.Sprintf("%s/%s/messages", graphBaseURL(), c.phoneNumberID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("whatsapp cloud send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("whatsapp cloud send: graph API %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// chunkText splits s into pieces of at most max runes, preferring to break on a
// newline near the limit so messages don't split mid-line.
func chunkText(s string, max int) []string {
	r := []rune(s)
	if len(r) <= max {
		return []string{s}
	}
	var out []string
	for len(r) > 0 {
		end := max
		if end > len(r) {
			end = len(r)
		} else {
			// Try to break on the last newline within the window.
			for i := end - 1; i > max/2; i-- {
				if r[i] == '\n' {
					end = i + 1
					break
				}
			}
		}
		out = append(out, string(r[:end]))
		r = r[end:]
	}
	return out
}
