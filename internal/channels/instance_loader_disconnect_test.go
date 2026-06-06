package channels

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// deletedRowStore simulates a store whose row has been deleted: Get always
// fails. Only Get is exercised by RestartInstance's delete path, so the other
// interface methods are left nil (embedding) and never called.
type deletedRowStore struct {
	store.ChannelInstanceStore
}

func (deletedRowStore) Get(context.Context, uuid.UUID) (*store.ChannelInstanceData, error) {
	return nil, errors.New("not found")
}

// TestRestartInstance_DeletedRow_TargetedStop verifies that disconnecting a
// channel (its DB row already gone) stops and unregisters ONLY that channel via
// the id→name map — without touching other loaded channels. This is the
// targeted-disconnect path that replaced the old full-Reload-on-delete.
func TestRestartInstance_DeletedRow_TargetedStop(t *testing.T) {
	msgBus := bus.New()
	mgr := NewManager(msgBus)
	loader := NewInstanceLoader(deletedRowStore{}, nil, mgr, msgBus, nil)

	gone := newTimeoutTestChannel("telegram-gone", TypeTelegram, false)
	keep := newTimeoutTestChannel("telegram-keep", TypeTelegram, false)
	loader.RegisterFactory(TypeTelegram, func(name string, _ json.RawMessage, _ json.RawMessage, _ *bus.MessageBus, _ store.PairingStore) (Channel, error) {
		if name == "telegram-gone" {
			return gone, nil
		}
		return keep, nil
	})

	goneID := uuid.New()
	keepID := uuid.New()
	loader.mu.Lock()
	_ = loader.loadInstance(context.Background(), store.ChannelInstanceData{BaseModel: store.BaseModel{ID: goneID}, Name: "telegram-gone", ChannelType: TypeTelegram}, true)
	_ = loader.loadInstance(context.Background(), store.ChannelInstanceData{BaseModel: store.BaseModel{ID: keepID}, Name: "telegram-keep", ChannelType: TypeTelegram}, true)
	loader.mu.Unlock()

	// Disconnect the deleted instance.
	loader.RestartInstance(context.Background(), goneID)

	// The deleted channel is stopped and unregistered...
	if gone.stopCalls.Load() == 0 {
		t.Fatal("expected Stop() on the disconnected channel")
	}
	if _, ok := mgr.GetChannel("telegram-gone"); ok {
		t.Fatal("disconnected channel should be unregistered from the manager")
	}
	if _, ok := loader.loaded["telegram-gone"]; ok {
		t.Fatal("disconnected channel should be removed from loaded set")
	}
	if _, ok := loader.byID[goneID]; ok {
		t.Fatal("disconnected channel should be removed from id→name map")
	}

	// ...while the other channel is completely untouched (no full reload).
	if keep.stopCalls.Load() != 0 {
		t.Fatal("unrelated channel must NOT be stopped by a targeted disconnect")
	}
	if _, ok := mgr.GetChannel("telegram-keep"); !ok {
		t.Fatal("unrelated channel should remain registered")
	}
}
