package whatsappcloud

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// credentials is the secret blob stored (encrypted) on the channel_instance.
type credentials struct {
	AccessToken   string `json:"access_token"`
	PhoneNumberID string `json:"phone_number_id"`
	AppSecret     string `json:"app_secret"`
	VerifyToken   string `json:"verify_token"`
	APIVersion    string `json:"api_version,omitempty"`
}

// instanceConfig is the non-secret per-instance config blob.
type instanceConfig struct {
	AllowFrom []string `json:"allow_from,omitempty"`
}

// Factory builds whatsapp_cloud channels from a channel_instance row.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, _ store.PairingStore) (channels.Channel, error) {

	var cr credentials
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, fmt.Errorf("decode whatsapp_cloud credentials: %w", err)
		}
	}
	var ic instanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("decode whatsapp_cloud config: %w", err)
		}
	}

	ch, err := New(Config{
		AccessToken:   cr.AccessToken,
		PhoneNumberID: cr.PhoneNumberID,
		AppSecret:     cr.AppSecret,
		VerifyToken:   cr.VerifyToken,
		APIVersion:    cr.APIVersion,
		AllowFrom:     ic.AllowFrom,
	}, msgBus)
	if err != nil {
		return nil, err
	}
	ch.SetName(name)
	return ch, nil
}
