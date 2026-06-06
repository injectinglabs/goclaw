package whatsappcloud

import (
	"log/slog"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// webhookPayload mirrors the WhatsApp Cloud API inbound notification shape.
// Only the fields we consume are modeled.
type webhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"` // WABA id
		Changes []struct {
			Field string `json:"field"` // "messages"
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// dispatchPayload routes every message in the notification to the channel
// registered for its phone_number_id, publishing inbound messages to the bus.
func dispatchPayload(p webhookPayload) {
	for _, entry := range p.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue // status updates, etc. — ignored for now
			}
			pnid := change.Value.Metadata.PhoneNumberID
			v, ok := webhookChannels.Load(pnid)
			if !ok {
				slog.Debug("whatsapp cloud: no channel for phone_number_id", "phone_number_id", pnid)
				continue
			}
			ch := v.(*Channel)
			for _, m := range change.Value.Messages {
				ch.handleInbound(m.From, m.Type, m.Text.Body)
			}
		}
	}
}

// handleInbound applies the allowlist and publishes a text message to the bus.
// Non-text types are skipped for now (media is a follow-up).
func (c *Channel) handleInbound(from, msgType, body string) {
	if msgType != "text" || body == "" {
		slog.Debug("whatsapp cloud: skipping non-text/empty message", "type", msgType, "from", from)
		return
	}
	if !c.IsAllowed(from) {
		slog.Debug("whatsapp cloud: sender not allowed", "from", from)
		return
	}
	c.Bus().PublishInbound(bus.InboundMessage{
		Channel:      c.Name(),
		SenderID:     from,
		ChatID:       from, // 1:1 chat keyed by the sender's wa_id
		Content:      body,
		PeerKind:     "direct",
		HistoryLimit: c.HistoryLimit(),
		TenantID:     c.TenantID(),
		CreatedBy:    c.CreatedBy(),
	})
}
