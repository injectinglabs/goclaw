// Package whatsappcloud implements the OFFICIAL Meta WhatsApp Business Cloud
// API channel (webhook inbound + Graph API outbound). This is distinct from the
// `whatsapp` package, which uses the unofficial whatsmeow (QR-link) protocol.
//
// Inbound: Meta POSTs messages to one shared webhook route (see webhook.go),
// routed to the right instance by phone_number_id. Outbound: HTTP POST to the
// Graph API with the instance's permanent access token.
package whatsappcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

const (
	defaultAPIVersion = "v21.0"
	maxMessageLen     = 4096 // WhatsApp text body limit
)

// Config carries everything a whatsapp_cloud instance needs. Secrets come from
// the encrypted channel_instance credentials; AllowFrom from its config.
type Config struct {
	AccessToken   string
	PhoneNumberID string
	AppSecret     string
	VerifyToken   string
	APIVersion    string // optional; defaults to defaultAPIVersion
	AllowFrom     []string
}

// Channel is a single connected WhatsApp Business number.
type Channel struct {
	*channels.BaseChannel

	accessToken   string
	phoneNumberID string
	appSecret     string
	verifyToken   string
	apiVersion    string

	running    atomic.Bool
	httpClient *http.Client
}

// New constructs a whatsapp_cloud Channel.
func New(cfg Config, msgBus *bus.MessageBus) (*Channel, error) {
	if cfg.AccessToken == "" || cfg.PhoneNumberID == "" {
		return nil, fmt.Errorf("whatsapp_cloud: access_token and phone_number_id are required")
	}
	apiVer := cfg.APIVersion
	if apiVer == "" {
		apiVer = defaultAPIVersion
	}
	base := channels.NewBaseChannel(channels.TypeWhatsAppCloud, msgBus, cfg.AllowFrom)
	return &Channel{
		BaseChannel:   base,
		accessToken:   cfg.AccessToken,
		phoneNumberID: cfg.PhoneNumberID,
		appSecret:     cfg.AppSecret,
		verifyToken:   cfg.VerifyToken,
		apiVersion:    apiVer,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Start registers the instance in the webhook dispatch registry (keyed by
// phone_number_id) so inbound messages route to it. No connection to open —
// Meta pushes over the shared webhook.
func (c *Channel) Start(_ context.Context) error {
	registerChannel(c.phoneNumberID, c)
	c.running.Store(true)
	return nil
}

// Stop deregisters the instance from the webhook registry.
func (c *Channel) Stop(_ context.Context) error {
	unregisterChannel(c.phoneNumberID)
	c.running.Store(false)
	return nil
}

func (c *Channel) IsRunning() bool { return c.running.Load() }

// Send delivers an outbound reply via the Graph API. v0 sends text (chunked to
// the 4096 limit); media attachments are sent as a link line appended to the
// text (full media upload is a follow-up).
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if msg.ChatID == "" {
		return fmt.Errorf("whatsapp_cloud: empty recipient")
	}
	body := msg.Content
	for _, m := range msg.Media {
		if m.URL != "" {
			if body != "" {
				body += "\n"
			}
			body += m.URL
		}
	}
	if body == "" {
		return nil
	}
	for _, chunk := range chunkText(body, maxMessageLen) {
		if err := c.sendText(ctx, msg.ChatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendText POSTs a single text message to the Graph API.
func (c *Channel) sendText(ctx context.Context, to, text string) error {
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", c.apiVersion, c.phoneNumberID)
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "text",
		"text":              map[string]any{"preview_url": false, "body": text},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp_cloud send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("whatsapp_cloud send: graph API %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

// chunkText splits s into <=limit-byte pieces on rune boundaries.
func chunkText(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var out []string
	runes := []rune(s)
	cur := make([]rune, 0, limit)
	size := 0
	for _, r := range runes {
		rl := len(string(r))
		if size+rl > limit {
			out = append(out, string(cur))
			cur = cur[:0]
			size = 0
		}
		cur = append(cur, r)
		size += rl
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}
