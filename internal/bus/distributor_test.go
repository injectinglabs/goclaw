package bus

import (
	"sync"
	"testing"
)

// recordDistributor returns a distributor func plus a getter for what it saw.
func recordDistributor() (func(Event), func() []Event) {
	var mu sync.Mutex
	var seen []Event
	return func(e Event) {
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
		}, func() []Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]Event(nil), seen...)
		}
}

// Broadcast mirrors only cache-invalidation events to the distributor; other
// event payloads (e.g. high-frequency UI events) must never hit the wire.
func TestBroadcast_DistributesOnlyCacheInvalidate(t *testing.T) {
	mb := New()
	defer mb.Close()

	var localCount int
	mb.Subscribe("sub", func(Event) { localCount++ })
	dist, seen := recordDistributor()
	mb.SetDistributor(dist)

	mb.Broadcast(Event{Name: "cache.invalidate", Payload: CacheInvalidatePayload{Kind: CacheKindChannelInstances, Key: "abc"}})
	mb.Broadcast(Event{Name: "agent", Payload: "streaming-token"}) // non-cache payload

	if localCount != 2 {
		t.Fatalf("local subscriber should see both events, got %d", localCount)
	}
	got := seen()
	if len(got) != 1 {
		t.Fatalf("distributor should see exactly the cache-invalidate event, got %d", len(got))
	}
	p, ok := got[0].Payload.(CacheInvalidatePayload)
	if !ok || p.Key != "abc" || p.Kind != CacheKindChannelInstances {
		t.Fatalf("distributor got wrong payload: %+v", got[0].Payload)
	}
}

// BroadcastLocal is the path the PG bridge uses when replaying a peer's
// notification: it must deliver locally but NOT re-distribute (no echo loop).
func TestBroadcastLocal_DoesNotDistribute(t *testing.T) {
	mb := New()
	defer mb.Close()

	var localCount int
	mb.Subscribe("sub", func(Event) { localCount++ })
	dist, seen := recordDistributor()
	mb.SetDistributor(dist)

	mb.BroadcastLocal(Event{Name: "cache.invalidate", Payload: CacheInvalidatePayload{Kind: CacheKindAgent, Key: "x"}})

	if localCount != 1 {
		t.Fatalf("local subscriber should see the event, got %d", localCount)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("BroadcastLocal must not distribute, distributor saw %d", len(got))
	}
}

// With no distributor installed (single-process / desktop), Broadcast is a
// pure local fan-out and must not panic.
func TestBroadcast_NoDistributor(t *testing.T) {
	mb := New()
	defer mb.Close()

	var localCount int
	mb.Subscribe("sub", func(Event) { localCount++ })
	mb.Broadcast(Event{Name: "cache.invalidate", Payload: CacheInvalidatePayload{Kind: CacheKindSkills}})

	if localCount != 1 {
		t.Fatalf("expected local delivery without distributor, got %d", localCount)
	}
}
