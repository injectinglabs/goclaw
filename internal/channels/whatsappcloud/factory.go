package whatsappcloud

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// cloudCreds maps the credentials JSON from the channel_instances table.
type cloudCreds struct {
	AccessToken   string `json:"access_token"`
	PhoneNumberID string `json:"phone_number_id"`
	WABAID        string `json:"waba_id,omitempty"`
}

// cloudInstanceConfig maps the non-secret config JSONB.
type cloudInstanceConfig struct {
	DMPolicy       string   `json:"dm_policy,omitempty"`
	RequireMention *bool    `json:"require_mention,omitempty"`
	HistoryLimit   int      `json:"history_limit,omitempty"`
	AllowFrom      []string `json:"allow_from,omitempty"`
}

// Factory returns a ChannelFactory for WhatsApp Cloud API instances.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, _ store.PairingStore) (channels.Channel, error) {

	var cr cloudCreds
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, fmt.Errorf("decode whatsapp_cloud credentials: %w", err)
		}
	}
	if cr.AccessToken == "" {
		return nil, fmt.Errorf("whatsapp_cloud: access_token is required")
	}
	if cr.PhoneNumberID == "" {
		return nil, fmt.Errorf("whatsapp_cloud: phone_number_id is required")
	}

	var ic cloudInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("decode whatsapp_cloud config: %w", err)
		}
	}

	ch := New(msgBus, cr.AccessToken, cr.PhoneNumberID, ic.AllowFrom)
	ch.SetName(name)
	if ic.HistoryLimit > 0 {
		ch.SetHistoryLimit(ic.HistoryLimit)
	}
	return ch, nil
}
